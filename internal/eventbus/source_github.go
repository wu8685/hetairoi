package eventbus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHubSource is a built-in poll Source over the GitHub REST API: every tick it
// lists issues and pull requests updated since the last poll and emits one event
// per (allowed) item. Unlike CodeHubPRSource it keeps state across ticks (the
// incremental `since` window and the resolved bot login), so it MUST be used via
// a pointer — buildFetch returns (&src).Fetch.
//
// Self-trigger guard: the agent stamps every comment it posts with a hidden
// marker (BotMarker). An item whose NEWEST comment carries the marker — i.e. the
// latest activity is the bot's own reply — is skipped. This is marker-based, not
// author-based, on purpose: the PAT owner and the human operator are usually the
// SAME GitHub account, so an author==bot check would also swallow the operator's
// own comments. The marker lets the watch react to any human comment (including
// the operator's) using the mutable updated_at as the Event.ID version, without
// the agent's own writes re-triggering it in a loop.
//
// Event.ID is `gh-<kind>-<number>-<activityUnixNano>`, so an unchanged item is
// deduped by the bus and each new non-bot activity fires exactly one turn. Keyed
// by number, repeated events on one item reuse the same session.
type GitHubSource struct {
	Repo         string       // "owner/name" (required)
	Token        string       // GitHub PAT; required for auth + the self-trigger guard
	APIBase      string       // default "https://api.github.com" (override for tests / GHE)
	State        string       // issue/PR state filter: open | closed | all (default "open")
	Kinds        string       // "both" | "issue" | "pr" (default "both")
	IssueType    string       // emitted Event.Type for issues (default "issue")
	PRType       string       // emitted Event.Type for PRs (default "pr")
	AllowNumbers map[int]bool // if non-empty, only these issue/PR numbers are emitted (blast-radius guard)
	BotMarker    string       // hidden marker the agent stamps on its comments (default "<!-- cma-agent -->")
	HTTP         *http.Client

	botLogin string    // token owner's login, resolved lazily on first Fetch (payload only)
	meTried  bool      // guard so a failed /user lookup isn't retried every tick
	since    time.Time // incremental window: only items updated at/after this are listed
}

type ghUser struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Body      string    `json:"body"`
	User      ghUser    `json:"user"`
	Labels    []ghLabel `json:"labels"`
	Comments  int       `json:"comments"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// PullRequest is present (non-nil) iff this "issue" is actually a PR — the
	// /issues endpoint returns both.
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

type ghComment struct {
	ID        int64     `json:"id"`
	User      ghUser    `json:"user"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ghPull struct {
	Draft          bool       `json:"draft"`
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	MergedAt       *time.Time `json:"merged_at"`
	Head           struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (s *GitHubSource) apiBase() string {
	if s.APIBase != "" {
		return s.APIBase
	}
	return "https://api.github.com"
}

func (s *GitHubSource) state() string {
	if s.State != "" {
		return s.State
	}
	return "open"
}

func (s *GitHubSource) issueType() string {
	if s.IssueType != "" {
		return s.IssueType
	}
	return "issue"
}

func (s *GitHubSource) prType() string {
	if s.PRType != "" {
		return s.PRType
	}
	return "pr"
}

func (s *GitHubSource) botMarker() string {
	if s.BotMarker != "" {
		return s.BotMarker
	}
	return "<!-- cma-agent -->"
}

func (s *GitHubSource) wants(isPR bool) bool {
	switch s.Kinds {
	case "issue":
		return !isPR
	case "pr":
		return isPR
	default: // "both" / ""
		return true
	}
}

func (s *GitHubSource) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Fetch implements FetchFunc: list issues+PRs updated since the last tick, apply
// the self-trigger guard, and build one event per (allowed) item.
func (s *GitHubSource) Fetch(ctx context.Context) ([]Event, error) {
	if s.botLogin == "" && !s.meTried && s.Token != "" {
		s.meTried = true
		if login, err := s.me(ctx); err == nil {
			s.botLogin = login
		}
	}
	tickStart := time.Now()
	issues, err := s.listIssues(ctx, s.since)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, it := range issues {
		if len(s.AllowNumbers) > 0 && !s.AllowNumbers[it.Number] {
			continue
		}
		isPR := it.PullRequest != nil
		if !s.wants(isPR) {
			continue
		}

		// Fetch the newest comment (if any) for the self-trigger guard and payload.
		activity := it.UpdatedAt
		var latest *ghComment
		if it.Comments > 0 {
			c, err := s.latestComment(ctx, it.Number, it.Comments)
			if err != nil {
				return out, fmt.Errorf("issue %d comments: %w", it.Number, err)
			}
			if c != nil {
				latest = c
				if c.UpdatedAt.After(activity) {
					activity = c.UpdatedAt
				}
			}
		}
		// Self-trigger guard (marker-based): skip when the newest comment is the
		// agent's own stamped reply, so its writes don't re-trigger it.
		if latest != nil && strings.Contains(latest.Body, s.botMarker()) {
			continue
		}

		fields := map[string]any{
			"number":     it.Number,
			"title":      it.Title,
			"state":      it.State,
			"body":       it.Body,
			"author":     it.User.Login,
			"url":        it.HTMLURL,
			"labels":     labelNames(it.Labels),
			"comments":   it.Comments,
			"kind":       kindStr(isPR),
			"repo":       s.Repo,
			"updated_at": it.UpdatedAt.UTC().Format(time.RFC3339),
			"bot_login":  s.botLogin,
			"marker":     s.botMarker(),
		}
		if latest != nil {
			fields["latest_comment"] = map[string]any{
				"author":     latest.User.Login,
				"body":       latest.Body,
				"created_at": latest.CreatedAt.UTC().Format(time.RFC3339),
			}
		}

		evType := s.issueType()
		if isPR {
			evType = s.prType()
			// Best-effort PR detail; a failure here shouldn't drop the event.
			if pr, err := s.pull(ctx, it.Number); err == nil {
				fields["head_sha"] = pr.Head.SHA
				fields["head_ref"] = pr.Head.Ref
				fields["base_ref"] = pr.Base.Ref
				fields["draft"] = pr.Draft
				fields["mergeable_state"] = pr.MergeableState
				if pr.Mergeable != nil {
					fields["mergeable"] = *pr.Mergeable
				}
				if pr.MergedAt != nil {
					fields["state"] = "merged"
				}
			}
		}

		payload, _ := json.Marshal(fields)
		out = append(out, Event{
			ID:      fmt.Sprintf("gh-%s-%d-%d", kindStr(isPR), it.Number, activity.UnixNano()),
			Type:    evType,
			Subject: strconv.Itoa(it.Number),
			Payload: payload,
			Source:  "github:" + s.Repo,
			Time:    tickStart,
		})
	}
	// Advance the window to the tick start (captured before listing) so activity
	// during this tick is caught next time; the bus dedups any overlap.
	s.since = tickStart
	return out, nil
}

func (s *GitHubSource) listIssues(ctx context.Context, since time.Time) ([]ghIssue, error) {
	q := url.Values{}
	q.Set("state", s.state())
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	q.Set("per_page", "50")
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	var issues []ghIssue
	err := s.getJSON(ctx, "/repos/"+s.Repo+"/issues?"+q.Encode(), &issues)
	return issues, err
}

// latestComment returns the newest comment on an issue. Issue comments come back
// ascending (oldest first) with no sort option, so we jump straight to the last
// page using the known total count and take its last element — one request.
func (s *GitHubSource) latestComment(ctx context.Context, number, count int) (*ghComment, error) {
	if count <= 0 {
		return nil, nil
	}
	page := (count + 99) / 100
	var cs []ghComment
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", s.Repo, number, page)
	if err := s.getJSON(ctx, path, &cs); err != nil {
		return nil, err
	}
	if len(cs) == 0 {
		return nil, nil
	}
	return &cs[len(cs)-1], nil
}

func (s *GitHubSource) pull(ctx context.Context, number int) (*ghPull, error) {
	var pr ghPull
	if err := s.getJSON(ctx, fmt.Sprintf("/repos/%s/pulls/%d", s.Repo, number), &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (s *GitHubSource) me(ctx context.Context) (string, error) {
	var u ghUser
	if err := s.getJSON(ctx, "/user", &u); err != nil {
		return "", err
	}
	return u.Login, nil
}

func (s *GitHubSource) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBase()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, bytes.TrimSpace(b))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func labelNames(ls []ghLabel) []string {
	if len(ls) == 0 {
		return nil
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func kindStr(isPR bool) string {
	if isPR {
		return "pr"
	}
	return "issue"
}

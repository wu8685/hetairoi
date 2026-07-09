package eventbus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GitHubSource is a built-in poll Source over the GitHub REST API, purpose-built
// for the issue → code → PR → review → fix → merge agent loop. Every tick it
// lists issues+PRs updated since the last poll and emits, per (allowed) item,
// one of three event types — each with its own dedup key so a transition fires
// exactly once:
//
//	issue      id gh-issue-<n>-<activity>       a labeled issue changed
//	pr.push    id gh-pr-<n>-push-<head_sha>     new commits on a PR (open/synchronize)
//	pr.review  id gh-pr-<n>-review-<review_id>  a review was submitted
//
// The split is what lets one GitHub account drive the whole loop without an
// author-identity check: the coder only ever produces pr.push, the reviewer only
// ever produces pr.review, so routing on the event TYPE (plus review_verdict)
// never self-triggers. Payload carries the routing fields handlers match on:
// has_agent_build_label, authorized, is_agent_pr, review_verdict, issue_ref.
//
// # Trust boundary (issue ingestion is an auto-execute attack surface)
//
// The label→code→PR loop runs untended, so any path that shapes issue content,
// labels, or comments can try to steer an agent. The source enforces a hard
// trust boundary at ingestion so downstream agents only ever see owner-authored
// instructions:
//
//   - owner-only content: only comments authored by the repo owner are surfaced
//     as instructions (latest_comment); a non-owner comment is never fed to an
//     agent (and never becomes the trigger), it is only logged as a probe.
//   - approval gate: the agent-build label is a CANDIDATE, not authorization.
//     `authorized` is true only when the label is present AND the intent is
//     owner-backed — the issue is owner-authored, or an owner posted an explicit
//     approval comment. A non-owner-driven label yields authorized=false, logged.
//   - owner-only review verdicts: a pr.review event is emitted only for a verdict
//     comment authored by the owner; a non-owner verdict is ignored and logged.
//
// It keeps state across ticks (the incremental `since` window, resolved bot
// login), so it MUST be used via a pointer — buildFetch returns (&src).Fetch.
type GitHubSource struct {
	Repo          string       // "owner/name" (required)
	Owner         string       // trusted repo owner login; defaults to the owner segment of Repo
	Token         string       // GitHub PAT; required (auth + rate limit + GET /user)
	APIBase       string       // default "https://api.github.com" (override for tests / GHE)
	State         string       // issue/PR state filter: open | closed | all (default "open")
	Kinds         string       // "both" | "issue" | "pr" (default "both")
	IssueType     string       // emitted type for issue events (default "issue")
	PushType      string       // emitted type for PR-commit events (default "pr.push")
	ReviewType    string       // emitted type for PR-review events (default "pr.review")
	BuildLabel    string       // label that opts an issue into the loop (default "agent-build")
	AgentPrefix   string       // head-branch prefix marking a loop PR (default "agent/")
	ApproveMarker string       // owner approval marker authorizing a non-owner issue (default "<!-- cma-approve -->")
	AllowNumbers  map[int]bool // if non-empty, only these issue/PR numbers are emitted
	BotMarker     string       // hidden marker the agent stamps on issue comments (default "<!-- cma-agent -->")
	HTTP          *http.Client
	Logf          func(string, ...any) // trust-boundary trace sink; defaults to log.Printf

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

// Review verdict markers: with a single GitHub account the reviewer cannot submit
// a native APPROVE / REQUEST_CHANGES review on the account's OWN PR (GitHub blocks
// self-approval), so the reviewer instead posts a PR comment ending with one of
// these markers, and the source routes on the parsed verdict.
const (
	reviewApproveMarker = "<!-- cma-review:approved -->"
	reviewChangesMarker = "<!-- cma-review:changes -->"
)

// parseVerdict extracts the reviewer's verdict from a PR comment body.
func parseVerdict(body string) string {
	switch {
	case strings.Contains(body, reviewApproveMarker):
		return "approved"
	case strings.Contains(body, reviewChangesMarker):
		return "changes"
	default:
		return ""
	}
}

// issueRefRe extracts the issue a PR closes from its body via GitHub's closing
// keywords ("Fixes #12", "closes #7", …), so a fix event can be keyed back to the
// coder's original issue session.
var issueRefRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)`)

func parseIssueRef(body string) string {
	if m := issueRefRe.FindStringSubmatch(body); len(m) == 2 {
		return m[1]
	}
	return ""
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

func (s *GitHubSource) issueType() string   { return orDefault(s.IssueType, "issue") }
func (s *GitHubSource) pushType() string    { return orDefault(s.PushType, "pr.push") }
func (s *GitHubSource) reviewType() string  { return orDefault(s.ReviewType, "pr.review") }
func (s *GitHubSource) buildLabel() string    { return orDefault(s.BuildLabel, "agent-build") }
func (s *GitHubSource) agentPrefix() string   { return orDefault(s.AgentPrefix, "agent/") }
func (s *GitHubSource) botMarker() string     { return orDefault(s.BotMarker, "<!-- cma-agent -->") }
func (s *GitHubSource) approveMarker() string { return orDefault(s.ApproveMarker, "<!-- cma-approve -->") }

// owner is the trusted author login. It defaults to the owner segment of Repo
// ("owner/name" → "owner"), so the common case needs no extra config.
func (s *GitHubSource) owner() string {
	if s.Owner != "" {
		return s.Owner
	}
	if i := strings.IndexByte(s.Repo, '/'); i > 0 {
		return s.Repo[:i]
	}
	return s.Repo
}

// logf leaves a trace when a trust-boundary check rejects something, so a probe
// of the auto-execute entry point is visible. Tests inject Logf to capture it.
func (s *GitHubSource) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf("eventbus: github %s: "+format, append([]any{s.Repo}, args...)...)
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
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

// Fetch implements FetchFunc: list issues+PRs updated since the last tick and
// build the loop's typed events.
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
		if isPR {
			evs, err := s.prEvents(ctx, it, tickStart)
			if err != nil {
				return out, err
			}
			out = append(out, evs...)
			continue
		}
		if ev, ok, err := s.issueEvent(ctx, it, tickStart); err != nil {
			return out, err
		} else if ok {
			out = append(out, ev)
		}
	}
	// Advance the window to the tick start (captured before listing) so activity
	// during this tick is caught next time; the bus dedups any overlap.
	s.since = tickStart
	return out, nil
}

// issueEvent builds the `issue` event for a non-PR item. It enforces the trust
// boundary: only the repo owner's comments count as instructions (a non-owner
// comment is never surfaced and never becomes the trigger), the agent-build
// label alone is not authorization (see authorized), and the marker self-trigger
// guard still skips when the newest comment is the agent's own reply.
func (s *GitHubSource) issueEvent(ctx context.Context, it ghIssue, t time.Time) (Event, bool, error) {
	owner := s.owner()
	activity := it.UpdatedAt

	var comments []ghComment
	if it.Comments > 0 {
		cs, err := s.commentsLastPage(ctx, it.Number, it.Comments)
		if err != nil {
			return Event{}, false, fmt.Errorf("issue %d comments: %w", it.Number, err)
		}
		comments = cs
	}

	// newest comment overall drives the self-trigger guard; latestOwner is the
	// newest OWNER comment, the only one trusted as an instruction / trigger.
	var newest, latestOwner *ghComment
	ownerApproved := false
	for i := range comments {
		c := &comments[i]
		newest = c
		if c.User.Login == owner {
			latestOwner = c
			if strings.Contains(c.Body, s.approveMarker()) {
				ownerApproved = true
			}
		}
	}

	if newest != nil && strings.Contains(newest.Body, s.botMarker()) {
		return Event{}, false, nil // agent's own reply is newest → don't re-trigger
	}
	if newest != nil && newest.User.Login != owner {
		// A non-owner comment is untrusted: not fed to the agent, not a trigger.
		s.logf("issue #%d: ignoring untrusted comment from %q (owner-only trust)", it.Number, newest.User.Login)
	}
	// Only owner-authored comments advance the activity window / dedup key, so a
	// stranger's comment never spawns a fresh agent turn on its own.
	if latestOwner != nil && latestOwner.UpdatedAt.After(activity) {
		activity = latestOwner.UpdatedAt
	}

	hasBuild := hasLabel(it.Labels, s.buildLabel())
	// Approval gate: the label is only a candidate. Authorize the build only when
	// the intent is owner-backed — an owner-authored issue, or an owner approval
	// comment on a non-owner issue. A non-owner-driven label is refused + logged.
	authorized := hasBuild && (it.User.Login == owner || ownerApproved)
	if hasBuild && !authorized {
		s.logf("issue #%d: %q label present but unauthorized (author=%q, no owner approval) — refusing to dispatch build",
			it.Number, s.buildLabel(), it.User.Login)
	}

	fields := map[string]any{
		"number":                it.Number,
		"title":                 it.Title,
		"state":                 it.State,
		"body":                  it.Body,
		"author":                it.User.Login,
		"url":                   it.HTMLURL,
		"labels":                labelNames(it.Labels),
		"has_agent_build_label": hasBuild,
		"authorized":            authorized,
		"owner":                 owner,
		"kind":                  "issue",
		"repo":                  s.Repo,
		"updated_at":            it.UpdatedAt.UTC().Format(time.RFC3339),
		"bot_login":             s.botLogin,
		"marker":                s.botMarker(),
	}
	if latestOwner != nil {
		fields["latest_comment"] = map[string]any{
			"author":     latestOwner.User.Login,
			"body":       latestOwner.Body,
			"created_at": latestOwner.CreatedAt.UTC().Format(time.RFC3339),
		}
	}
	payload, _ := json.Marshal(fields)
	return Event{
		ID:      fmt.Sprintf("gh-issue-%d-%d", it.Number, activity.UnixNano()),
		Type:    s.issueType(),
		Subject: strconv.Itoa(it.Number),
		Payload: payload,
		Source:  "github:" + s.Repo,
		Time:    t,
	}, true, nil
}

// prEvents builds up to two events for a PR: pr.push (keyed by head sha) and, if
// a review has been submitted, pr.review (keyed by the latest review id).
func (s *GitHubSource) prEvents(ctx context.Context, it ghIssue, t time.Time) ([]Event, error) {
	pr, err := s.pull(ctx, it.Number)
	if err != nil {
		return nil, fmt.Errorf("pr %d detail: %w", it.Number, err)
	}
	issueRef := parseIssueRef(it.Body)
	isAgentPR := strings.HasPrefix(pr.Head.Ref, s.agentPrefix())
	base := map[string]any{
		"number":      it.Number,
		"title":       it.Title,
		"url":         it.HTMLURL,
		"author":      it.User.Login,
		"head_sha":    pr.Head.SHA,
		"head_ref":    pr.Head.Ref,
		"base_ref":    pr.Base.Ref,
		"issue_ref":   issueRef,
		"is_agent_pr": isAgentPR,
		"draft":       pr.Draft,
		"state":       it.State,
		"repo":        s.Repo,
		"marker":      s.botMarker(),
	}
	if pr.Mergeable != nil {
		base["mergeable"] = *pr.Mergeable
	}
	if pr.MergedAt != nil {
		base["merged"] = true
	}

	var out []Event
	// pr.push — one per head sha (a re-push yields a new sha → new event).
	pushPayload, _ := json.Marshal(withKV(base, "event", "push"))
	out = append(out, Event{
		ID:      fmt.Sprintf("gh-pr-%d-push-%s", it.Number, pr.Head.SHA),
		Type:    s.pushType(),
		Subject: strconv.Itoa(it.Number),
		Payload: pushPayload,
		Source:  "github:" + s.Repo,
		Time:    t,
	})

	// pr.review — routed off the OWNER's latest verdict comment (see the
	// self-approval note on the verdict markers). Trust boundary: only an
	// owner-authored verdict is acted on; a stranger's "approved"/"changes"
	// comment is ignored (and logged), so it can't steer the loop. One event per
	// comment id, so a new verdict fires once and an unchanged one is deduped.
	if it.Comments > 0 {
		cs, err := s.commentsLastPage(ctx, it.Number, it.Comments)
		if err != nil {
			return out, fmt.Errorf("pr %d comments: %w", it.Number, err)
		}
		owner := s.owner()
		if c := latestOwnerVerdict(cs, owner, s.logf, it.Number); c != nil {
			verdict := parseVerdict(c.Body)
			rp := withKV(base, "event", "review")
			rp["review_verdict"] = verdict
			rp["reviewer"] = c.User.Login
			rp["review_body"] = c.Body
			reviewPayload, _ := json.Marshal(rp)
			out = append(out, Event{
				ID:      fmt.Sprintf("gh-pr-%d-review-%d", it.Number, c.ID),
				Type:    s.reviewType(),
				Subject: strconv.Itoa(it.Number),
				Payload: reviewPayload,
				Source:  "github:" + s.Repo,
				Time:    t,
			})
		}
	}
	return out, nil
}

// latestOwnerVerdict returns the newest OWNER-authored comment carrying a review
// verdict, scanning newest-first. A verdict comment from a non-owner is logged
// and skipped — it must never route the loop.
func latestOwnerVerdict(cs []ghComment, owner string, logf func(string, ...any), number int) *ghComment {
	for i := len(cs) - 1; i >= 0; i-- {
		c := &cs[i]
		if parseVerdict(c.Body) == "" {
			continue
		}
		if c.User.Login != owner {
			if logf != nil {
				logf("PR #%d: ignoring review verdict from non-owner %q (owner-only trust)", number, c.User.Login)
			}
			continue
		}
		return c
	}
	return nil
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

// commentsLastPage returns the newest page of comments on an issue/PR (ascending,
// oldest-first within the page). Issue comments come back ascending with no sort
// option, so we jump straight to the last page using the known total count — one
// request. Callers scan it newest-first for the marker guard, the latest owner
// comment, and the latest owner verdict.
func (s *GitHubSource) commentsLastPage(ctx context.Context, number, count int) ([]ghComment, error) {
	if count <= 0 {
		return nil, nil
	}
	page := (count + 99) / 100
	var cs []ghComment
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", s.Repo, number, page)
	if err := s.getJSON(ctx, path, &cs); err != nil {
		return nil, err
	}
	return cs, nil
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

func hasLabel(ls []ghLabel, name string) bool {
	for _, l := range ls {
		if l.Name == name {
			return true
		}
	}
	return false
}

// withKV returns a shallow copy of m with k=v added, so the shared base payload
// isn't mutated across the push/review events.
func withKV(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for kk, vv := range m {
		out[kk] = vv
	}
	out[k] = v
	return out
}

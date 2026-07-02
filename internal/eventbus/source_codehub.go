package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"time"
)

// CodeHubPRSource is a built-in poll Source: it lists open CodeHub pull requests
// where the configured reviewer is a reviewer, and emits one event per PR keyed
// by its head commit. Because Event.ID encodes the head sha, an unchanged PR is
// deduped by the bus (no repeated turns), while a re-pushed PR (the author's
// fix) produces a fresh ID and re-triggers review. Posting a comment does not
// change the head sha, so the source never re-triggers on its own writes.
//
// It shells out to the CodeHub CLI (v1.x), which carries its own auth; the
// process inherits the operator's login and ssh keys.
type CodeHubPRSource struct {
	Bin       string       // codehub binary (default "codehub")
	Project   string       // namespace/project, e.g. example-org/k8s-extension
	Reviewer  string       // reviewer filter, e.g. "@me" (empty = no reviewer filter)
	Author    string       // author filter, e.g. "@me" (empty = no author filter)
	State     string       // PR state (default "opened")
	EventType string       // emitted Event.Type (default "pr")
	AllowIIDs map[int]bool // if non-empty, only these PR iids are emitted (blast-radius guard)
	// MergeStatus, when true, fetches each PR's mergeable/merge_status (one extra
	// `pr show` per PR) into the payload AND folds merge_status into Event.ID — so
	// a PR that BECOMES conflicted (e.g. target advanced) yields a fresh event even
	// though its head sha is unchanged. Used by the conflict-fixer watch.
	MergeStatus bool
}

type codehubPR struct {
	IID          int    `json:"iid"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	URL          string `json:"url"`
	Author       struct {
		Name     string `json:"name"`
		Username string `json:"username"`
	} `json:"author"`
	Source struct {
		PathWithNamespace string `json:"path_with_namespace"`
		SSHURL            string `json:"ssh_url"`
		HTTPURL           string `json:"http_url"`
	} `json:"source"`
}

type codehubCommit struct {
	ID            string `json:"id"`
	CommittedDate string `json:"committed_date"`
}

func (s CodeHubPRSource) bin() string {
	if s.Bin != "" {
		return s.Bin
	}
	return "codehub"
}

func (s CodeHubPRSource) evType() string {
	if s.EventType != "" {
		return s.EventType
	}
	return "pr"
}

func (s CodeHubPRSource) state() string {
	if s.State != "" {
		return s.State
	}
	return "opened"
}

// Fetch implements FetchFunc: list reviewer PRs, resolve each head sha, and
// build one event per (allowed) PR.
func (s CodeHubPRSource) Fetch(ctx context.Context) ([]Event, error) {
	prs, err := s.listPRs(ctx)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, pr := range prs {
		if len(s.AllowIIDs) > 0 && !s.AllowIIDs[pr.IID] {
			continue
		}
		sha, err := s.headSHA(ctx, pr.IID)
		if err != nil {
			return out, fmt.Errorf("pr %d head sha: %w", pr.IID, err)
		}
		fields := map[string]any{
			"iid":           pr.IID,
			"title":         pr.Title,
			"description":   pr.Description,
			"source_branch": pr.SourceBranch,
			"target_branch": pr.TargetBranch,
			"head_sha":      sha,
			"author":        pr.Author.Username,
			"url":           pr.URL,
			"project":       s.Project,
			"ssh_url":       pr.Source.SSHURL,
			"http_url":      pr.Source.HTTPURL,
		}
		version := sha
		if s.MergeStatus {
			ms, mergeable, err := s.mergeStatus(ctx, pr.IID)
			if err != nil {
				return out, fmt.Errorf("pr %d merge status: %w", pr.IID, err)
			}
			fields["merge_status"] = ms
			fields["mergeable"] = mergeable
			version = sha + "-" + ms // conflict appearing -> new event even if head sha unchanged
		}
		payload, _ := json.Marshal(fields)
		out = append(out, Event{
			ID:      fmt.Sprintf("pr-%d-%s", pr.IID, version),
			Type:    s.evType(),
			Subject: strconv.Itoa(pr.IID),
			Payload: payload,
			Source:  "codehub:" + s.Project,
			Time:    time.Now(),
		})
	}
	return out, nil
}

func (s CodeHubPRSource) listPRs(ctx context.Context) ([]codehubPR, error) {
	args := []string{"pr", "list", "-P", s.Project, "--state", s.state(), "--json", "--no-pager"}
	if s.Reviewer != "" {
		args = append(args, "--reviewer", s.Reviewer)
	}
	if s.Author != "" {
		args = append(args, "--author", s.Author)
	}
	out, err := s.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var prs []codehubPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse pr list: %w", err)
	}
	return prs, nil
}

// headSHA returns the most recently committed sha of the PR (its head).
func (s CodeHubPRSource) headSHA(ctx context.Context, iid int) (string, error) {
	out, err := s.run(ctx, "pr", "commits", strconv.Itoa(iid), "-P", s.Project, "--json", "--no-pager")
	if err != nil {
		return "", err
	}
	var commits []codehubCommit
	if err := json.Unmarshal(out, &commits); err != nil {
		return "", fmt.Errorf("parse pr commits: %w", err)
	}
	if len(commits) == 0 {
		return "", fmt.Errorf("no commits")
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i].CommittedDate > commits[j].CommittedDate })
	return commits[0].ID, nil
}

// mergeStatus returns the PR's merge_status string and mergeable bool via
// `pr show --json`.
func (s CodeHubPRSource) mergeStatus(ctx context.Context, iid int) (string, bool, error) {
	out, err := s.run(ctx, "pr", "show", strconv.Itoa(iid), "-P", s.Project, "--json", "--no-pager")
	if err != nil {
		return "", false, err
	}
	var d struct {
		MergeStatus string `json:"merge_status"`
		Mergeable   bool   `json:"mergeable"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return "", false, fmt.Errorf("parse pr show: %w", err)
	}
	return d.MergeStatus, d.Mergeable, nil
}

func (s CodeHubPRSource) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, s.bin(), args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s %v: %s", s.bin(), args, string(ee.Stderr))
		}
		return nil, err
	}
	return out, nil
}

package eventbus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// ghFakeAPI is a minimal GitHub REST stand-in for the source tests.
type ghFakeAPI struct {
	me       string
	issues   []map[string]any         // /issues listing (issues + PRs)
	comments map[int][]map[string]any // issue/PR number -> comments (ascending)
	pulls    map[int]map[string]any   // PR number -> /pulls/{n} detail
}

func (f ghFakeAPI) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": f.me})
	})
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(f.issues)
	})
	mux.HandleFunc("/repos/o/r/issues/", func(w http.ResponseWriter, r *http.Request) {
		// /repos/o/r/issues/{n}/comments
		n := pathNum(r.URL.Path, "/repos/o/r/issues/")
		_ = json.NewEncoder(w).Encode(f.comments[n])
	})
	mux.HandleFunc("/repos/o/r/pulls/", func(w http.ResponseWriter, r *http.Request) {
		n := pathNum(r.URL.Path, "/repos/o/r/pulls/")
		body := f.pulls[n]
		if body == nil {
			body = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	return httptest.NewServer(mux)
}

// pathNum parses the first integer segment after prefix.
func pathNum(path, prefix string) int {
	seg := strings.SplitN(strings.TrimPrefix(path, prefix), "/", 2)[0]
	n, _ := strconv.Atoi(seg)
	return n
}

func newGHSource(base string) *GitHubSource {
	// Proxy disabled: httptest is on 127.0.0.1 and this box's ambient http_proxy
	// would otherwise hijack loopback (see local-loopback-proxy note).
	return &GitHubSource{Repo: "o/r", Token: "t", APIBase: base,
		HTTP: &http.Client{Transport: &http.Transport{Proxy: nil}}}
}

func eventsByType(evs []Event) map[string]Event {
	m := map[string]Event{}
	for _, e := range evs {
		m[e.Type] = e
	}
	return m
}

func payloadOf(e Event) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(e.Payload, &p)
	return p
}

func TestGitHubSource_IssueEmitsWithLabelFlag(t *testing.T) {
	f := ghFakeAPI{
		me: "wu8685",
		issues: []map[string]any{
			{"number": 5, "title": "please build X", "state": "open",
				"user": map[string]any{"login": "wu8685"}, "comments": 0,
				"labels":     []map[string]any{{"name": "agent-build"}, {"name": "bug"}},
				"updated_at": "2026-07-03T10:00:00Z"},
		},
	}
	srv := f.server()
	defer srv.Close()
	evs, err := newGHSource(srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "issue" {
		t.Fatalf("want 1 issue event, got %d: %+v", len(evs), evs)
	}
	p := payloadOf(evs[0])
	if p["has_agent_build_label"] != true {
		t.Errorf("has_agent_build_label = %v, want true", p["has_agent_build_label"])
	}
	if !strings.HasPrefix(evs[0].ID, "gh-issue-5-") {
		t.Errorf("id = %q, want gh-issue-5-*", evs[0].ID)
	}
}

func TestGitHubSource_PRPushAndReview(t *testing.T) {
	f := ghFakeAPI{
		me: "wu8685",
		issues: []map[string]any{
			{"number": 12, "title": "fix the thing", "state": "open",
				"user": map[string]any{"login": "wu8685"}, "comments": 2,
				"body":         "Implements the fix.\n\nFixes #5",
				"updated_at":   "2026-07-03T11:00:00Z",
				"pull_request": map[string]any{"url": "x"}},
		},
		pulls: map[int]map[string]any{
			12: {"head": map[string]any{"sha": "abc123", "ref": "agent/issue-5"},
				"base": map[string]any{"ref": "main"}, "draft": false},
		},
		// Reviewer's verdict lives in the latest PR comment (single-account: no
		// native self-approval), newest last.
		comments: map[int][]map[string]any{
			12: {
				{"id": 100, "user": map[string]any{"login": "wu8685"}, "body": "looking..."},
				{"id": 101, "user": map[string]any{"login": "wu8685"}, "body": "fix error handling\n\n<!-- cma-review:changes -->"},
			},
		},
	}
	srv := f.server()
	defer srv.Close()
	evs, err := newGHSource(srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	byType := eventsByType(evs)
	push, ok := byType["pr.push"]
	if !ok {
		t.Fatalf("no pr.push event; got %+v", evs)
	}
	if push.ID != "gh-pr-12-push-abc123" {
		t.Errorf("push id = %q, want gh-pr-12-push-abc123", push.ID)
	}
	pp := payloadOf(push)
	if pp["is_agent_pr"] != true {
		t.Errorf("is_agent_pr = %v, want true (head ref agent/…)", pp["is_agent_pr"])
	}
	if pp["issue_ref"] != "5" {
		t.Errorf("issue_ref = %v, want 5 (parsed from 'Fixes #5')", pp["issue_ref"])
	}

	rev, ok := byType["pr.review"]
	if !ok {
		t.Fatalf("no pr.review event; got %+v", evs)
	}
	if rev.ID != "gh-pr-12-review-101" {
		t.Errorf("review id = %q, want gh-pr-12-review-101 (latest verdict comment)", rev.ID)
	}
	rp := payloadOf(rev)
	if rp["review_verdict"] != "changes" {
		t.Errorf("review_verdict = %v, want changes", rp["review_verdict"])
	}
}

func TestGitHubSource_NonAgentPRFlaggedFalse(t *testing.T) {
	f := ghFakeAPI{
		me: "wu8685",
		issues: []map[string]any{
			{"number": 20, "title": "random human PR", "state": "open",
				"user": map[string]any{"login": "someone"}, "comments": 0, "body": "no ref",
				"updated_at": "2026-07-03T12:00:00Z", "pull_request": map[string]any{"url": "x"}},
		},
		pulls: map[int]map[string]any{
			20: {"head": map[string]any{"sha": "def456", "ref": "feature-x"}, "base": map[string]any{"ref": "main"}},
		},
	}
	srv := f.server()
	defer srv.Close()
	evs, err := newGHSource(srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	byType := eventsByType(evs)
	if _, ok := byType["pr.review"]; ok {
		t.Errorf("no review submitted → should be no pr.review event")
	}
	pp := payloadOf(byType["pr.push"])
	if pp["is_agent_pr"] != false {
		t.Errorf("is_agent_pr = %v, want false (head ref feature-x)", pp["is_agent_pr"])
	}
	if pp["issue_ref"] != "" {
		t.Errorf("issue_ref = %v, want empty", pp["issue_ref"])
	}
}

func TestGitHubSource_IssueMarkerGuardAndKinds(t *testing.T) {
	f := ghFakeAPI{
		me: "wu8685",
		issues: []map[string]any{
			{"number": 7, "title": "botlast", "state": "open", "user": map[string]any{"login": "wu8685"},
				"comments": 1, "updated_at": "2026-07-03T13:00:00Z"},
		},
		comments: map[int][]map[string]any{
			7: {{"id": 1, "user": map[string]any{"login": "wu8685"}, "body": "done\n<!-- cma-agent -->",
				"created_at": "2026-07-03T13:00:00Z", "updated_at": "2026-07-03T13:00:00Z"}},
		},
	}
	srv := f.server()
	defer srv.Close()

	// Marker on the newest comment → issue event suppressed.
	evs, _ := newGHSource(srv.URL).Fetch(context.Background())
	if len(evs) != 0 {
		t.Fatalf("marker guard: want 0 events, got %+v", evs)
	}

	// Kinds=pr → issues skipped entirely.
	s := newGHSource(srv.URL)
	s.Kinds = "pr"
	if evs, _ := s.Fetch(context.Background()); len(evs) != 0 {
		t.Fatalf("Kinds=pr should skip the issue, got %+v", evs)
	}
}

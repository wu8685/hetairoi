package eventbus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ghFakeAPI is a minimal GitHub REST stand-in for the source tests. issues is the
// /issues listing; comments maps issue number -> its comment list (ascending).
type ghFakeAPI struct {
	me       string
	issues   []map[string]any
	comments map[int][]map[string]any
	pulls    map[int]map[string]any
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
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/"), "/")
		var n int
		_, _ = fmtSscan(parts[0], &n)
		_ = json.NewEncoder(w).Encode(f.comments[n])
	})
	mux.HandleFunc("/repos/o/r/pulls/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/o/r/pulls/"), "/")
		var n int
		_, _ = fmtSscan(parts[0], &n)
		body := f.pulls[n]
		if body == nil {
			body = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	return httptest.NewServer(mux)
}

func fmtSscan(s string, n *int) (int, error) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}

func newGHSource(base string, f ghFakeAPI) *GitHubSource {
	return &GitHubSource{Repo: "o/r", Token: "t", APIBase: base, HTTP: base2client()}
}

// base2client returns a client with the proxy explicitly disabled: the test's
// httptest server is on 127.0.0.1, and this dev box's ambient http_proxy would
// otherwise hijack loopback (see the local-loopback-proxy note).
func base2client() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil}}
}

func idsOf(evs []Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func TestGitHubSource_EmitsIssueAndPR(t *testing.T) {
	f := ghFakeAPI{
		me: "bot",
		issues: []map[string]any{
			{"number": 1, "title": "a bug", "state": "open", "user": map[string]any{"login": "alice"},
				"comments": 0, "updated_at": "2026-07-02T10:00:00Z", "created_at": "2026-07-02T09:00:00Z"},
			{"number": 2, "title": "a pr", "state": "open", "user": map[string]any{"login": "bob"},
				"comments": 0, "updated_at": "2026-07-02T10:05:00Z", "created_at": "2026-07-02T09:30:00Z",
				"pull_request": map[string]any{"url": "x"}},
		},
		pulls: map[int]map[string]any{2: {"head": map[string]any{"sha": "deadbeef", "ref": "feat"}, "base": map[string]any{"ref": "main"}}},
	}
	srv := f.server()
	defer srv.Close()
	s := newGHSource(srv.URL, f)

	evs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %v", len(evs), idsOf(evs))
	}
	byType := map[string]Event{}
	for _, e := range evs {
		byType[e.Type] = e
	}
	if byType["issue"].Subject != "1" {
		t.Errorf("issue subject = %q, want 1", byType["issue"].Subject)
	}
	if byType["pr"].Subject != "2" {
		t.Errorf("pr subject = %q, want 2", byType["pr"].Subject)
	}
	if !strings.HasPrefix(byType["issue"].ID, "gh-issue-1-") {
		t.Errorf("issue ID = %q, want gh-issue-1-*", byType["issue"].ID)
	}
	if !strings.HasPrefix(byType["pr"].ID, "gh-pr-2-") {
		t.Errorf("pr ID = %q, want gh-pr-2-*", byType["pr"].ID)
	}
	// PR detail folded into payload.
	var p map[string]any
	_ = json.Unmarshal(byType["pr"].Payload, &p)
	if p["head_sha"] != "deadbeef" {
		t.Errorf("pr head_sha = %v, want deadbeef", p["head_sha"])
	}
}

func TestGitHubSource_SelfTriggerGuard(t *testing.T) {
	f := ghFakeAPI{
		me: "bot",
		issues: []map[string]any{
			// #1: latest comment is the bot's own → must be skipped.
			{"number": 1, "title": "botlast", "state": "open", "user": map[string]any{"login": "alice"},
				"comments": 2, "updated_at": "2026-07-02T12:00:00Z", "created_at": "2026-07-02T09:00:00Z"},
			// #2: latest comment is a human → must emit.
			{"number": 2, "title": "humanlast", "state": "open", "user": map[string]any{"login": "alice"},
				"comments": 1, "updated_at": "2026-07-02T12:01:00Z", "created_at": "2026-07-02T09:00:00Z"},
		},
		comments: map[int][]map[string]any{
			// newest comment carries the bot marker (same account as the human) → skip.
			1: {
				{"id": 10, "user": map[string]any{"login": "wu8685"}, "body": "help", "created_at": "2026-07-02T11:00:00Z", "updated_at": "2026-07-02T11:00:00Z"},
				{"id": 11, "user": map[string]any{"login": "wu8685"}, "body": "on it\n<!-- cma-agent -->", "created_at": "2026-07-02T12:00:00Z", "updated_at": "2026-07-02T12:00:00Z"},
			},
			// newest comment has no marker (a human reply) → emit, even though the
			// author is the same wu8685 account as the bot.
			2: {
				{"id": 20, "user": map[string]any{"login": "wu8685"}, "body": "still broken", "created_at": "2026-07-02T12:01:00Z", "updated_at": "2026-07-02T12:01:00Z"},
			},
		},
	}
	srv := f.server()
	defer srv.Close()
	s := newGHSource(srv.URL, f)

	evs, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("want 1 event (bot-last skipped), got %d: %v", len(evs), idsOf(evs))
	}
	if evs[0].Subject != "2" {
		t.Errorf("emitted subject = %q, want 2 (the human-last issue)", evs[0].Subject)
	}
}

func TestGitHubSource_AllowNumbersAndKinds(t *testing.T) {
	mkIssues := func() []map[string]any {
		return []map[string]any{
			{"number": 1, "title": "i", "state": "open", "user": map[string]any{"login": "alice"}, "comments": 0, "updated_at": "2026-07-02T10:00:00Z"},
			{"number": 2, "title": "p", "state": "open", "user": map[string]any{"login": "bob"}, "comments": 0, "updated_at": "2026-07-02T10:00:00Z", "pull_request": map[string]any{"url": "x"}},
		}
	}
	f := ghFakeAPI{me: "bot", issues: mkIssues(), pulls: map[int]map[string]any{2: {}}}
	srv := f.server()
	defer srv.Close()

	// Kinds=issue → only the issue.
	s := newGHSource(srv.URL, f)
	s.Kinds = "issue"
	evs, _ := s.Fetch(context.Background())
	if len(evs) != 1 || evs[0].Type != "issue" {
		t.Fatalf("Kinds=issue: want 1 issue, got %v", idsOf(evs))
	}

	// AllowNumbers={2} → only PR #2.
	s2 := newGHSource(srv.URL, f)
	s2.AllowNumbers = map[int]bool{2: true}
	evs2, _ := s2.Fetch(context.Background())
	if len(evs2) != 1 || evs2[0].Subject != "2" {
		t.Fatalf("AllowNumbers={2}: want PR #2, got %v", idsOf(evs2))
	}
}

# GitHub Source Lossless Pagination Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Hetairoi's built-in `GitHubSource` exhaust a bounded, validated GitHub repository-issues pagination window before advancing `since`.

**Architecture:** Keep pagination inside `GitHubSource`. First refactor the existing authenticated JSON GET into a header-returning private helper, then add pure Link parsing and URL validation primitives, and finally make `listIssues` traverse at most 10 pages of 100 items. The generic Poller, Bus, Handler, SourceSpec, restart watermark behavior, and event payloads remain unchanged.

**Tech Stack:** Go 1.24, standard library `net/http` and `net/url`, `httptest`, existing `internal/eventbus` package.

## Global Constraints

- Follow the approved spec: `docs/superpowers/specs/2026-07-23-github-source-pagination-design.md`.
- Pagination belongs to `GitHubSource`, never to an event Handler.
- The first page counts toward the fixed 10-page budget.
- Every page requests up to 100 items; page 10 advertising another page is an error.
- A failed page, unsafe Link, malformed Link, cycle, or budget exhaustion must not advance `since`.
- Never forward the GitHub bearer token to a different origin or endpoint.
- Do not add fields to `SourceSpec`; do not add dependencies.
- Do not implement restart cursor persistence, overlap, reconciliation, or exactly-once delivery.
- Use TDD: observe each targeted test fail before writing the corresponding implementation.
- Prefix all Go commands with `GO111MODULE=on`.

---

### Task 1: Preserve response headers in the authenticated JSON helper

**Files:**
- Modify: `internal/eventbus/source_github_test.go`
- Modify: `internal/eventbus/source_github.go:502-522`

**Interfaces:**
- Consumes: `GitHubSource.client()`, `GitHubSource.apiBase()`, existing request headers and error-body behavior.
- Produces:

```go
func (s *GitHubSource) getJSONURL(
    ctx context.Context,
    rawURL string,
    v any,
) (http.Header, error)
```

- Preserves:

```go
func (s *GitHubSource) getJSON(
    ctx context.Context,
    path string,
    v any,
) error
```

- [ ] **Step 1: Add a failing header-preservation test**

Append this test to `internal/eventbus/source_github_test.go`:

```go
func TestGitHubSource_GetJSONURLReturnsHeaders(t *testing.T) {
	const link = `<https://api.example.test/page/2>; rel="next"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t" {
			t.Errorf("Authorization = %q, want Bearer t", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		w.Header().Set("Link", link)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer srv.Close()

	s := newGHSource(srv.URL)
	var body map[string]bool
	headers, err := s.getJSONURL(context.Background(), srv.URL+"/page/1", &body)
	if err != nil {
		t.Fatalf("getJSONURL: %v", err)
	}
	if !body["ok"] {
		t.Fatalf("decoded body = %#v, want ok=true", body)
	}
	if got := headers.Get("Link"); got != link {
		t.Fatalf("Link = %q, want %q", got, link)
	}
}
```

- [ ] **Step 2: Run the new test and confirm the red state**

Run:

```sh
GO111MODULE=on go test ./internal/eventbus -run TestGitHubSource_GetJSONURLReturnsHeaders -count=1
```

Expected: compilation fails because `GitHubSource.getJSONURL` does not exist.

- [ ] **Step 3: Refactor the helper without changing existing callers**

Replace the current `getJSON` implementation in
`internal/eventbus/source_github.go` with:

```go
func (s *GitHubSource) getJSON(ctx context.Context, path string, v any) error {
	_, err := s.getJSONURL(ctx, s.apiBase()+path, v)
	return err
}

func (s *GitHubSource) getJSONURL(ctx context.Context, rawURL string, v any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("GET %s: %s: %s", rawURL, resp.Status, bytes.TrimSpace(b))
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return nil, err
	}
	return resp.Header.Clone(), nil
}
```

- [ ] **Step 4: Run the focused and existing GitHub tests**

Run:

```sh
GO111MODULE=on go test ./internal/eventbus -run 'TestGitHubSource' -count=1
```

Expected: all `TestGitHubSource*` tests pass.

- [ ] **Step 5: Commit the helper refactor**

```sh
git add internal/eventbus/source_github.go internal/eventbus/source_github_test.go
git commit -m "refactor: expose GitHub response headers"
```

---

### Task 2: Parse and validate GitHub next links before any authenticated request

**Files:**
- Modify: `internal/eventbus/source_github_test.go`
- Modify: `internal/eventbus/source_github.go`

**Interfaces:**
- Consumes: the initial absolute repository-issues URL and the raw `Link` header.
- Produces:

```go
func githubNextLink(header string) (rawURL string, ok bool, err error)
func validateGitHubNextURL(
    initialURL string,
    rawNext string,
    seen map[string]struct{},
) (canonicalURL string, err error)
func canonicalGitHubURL(u *url.URL) string
```

- [ ] **Step 1: Add failing Link parser tests**

Append these tests to `internal/eventbus/source_github_test.go`:

```go
func TestGitHubNextLink(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		want    string
		wantOK  bool
		wantErr bool
	}{
		{name: "empty"},
		{
			name:   "next among standard relations",
			header: `<https://api.github.test/issues?page=1>; rel="prev", <https://api.github.test/issues?page=3>; rel="next", <https://api.github.test/issues?page=9>; rel="last"`,
			want:   "https://api.github.test/issues?page=3",
			wantOK: true,
		},
		{
			name:   "well formed non-next relations",
			header: `<https://api.github.test/issues?page=1>; rel="prev", <https://api.github.test/issues?page=9>; rel="last"`,
		},
		{name: "malformed", header: "not-a-link", wantErr: true},
		{
			name:    "malformed parameters",
			header:  `<https://api.github.test/issues?page=2> rel="next"`,
			wantErr: true,
		},
		{
			name:    "multiple next relations",
			header:  `<https://api.github.test/issues?page=2>; rel="next", <https://api.github.test/issues?page=3>; rel="next"`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := githubNextLink(tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr=%v", err, tt.wantErr)
			}
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
```

- [ ] **Step 2: Add failing next-URL trust and cycle tests**

Add this test immediately after `TestGitHubNextLink`:

```go
func TestValidateGitHubNextURL(t *testing.T) {
	const initial = "https://ghe.example.test/api/v3/repos/o/r/issues?per_page=100&since=x"

	valid, err := url.Parse("https://ghe.example.test/api/v3/repos/o/r/issues?page=2&per_page=100&since=x")
	if err != nil {
		t.Fatal(err)
	}
	seenCycle := map[string]struct{}{canonicalGitHubURL(valid): {}}

	tests := []struct {
		name    string
		raw     string
		seen    map[string]struct{}
		want    string
		wantErr string
	}{
		{
			name: "valid same endpoint",
			raw:  "https://ghe.example.test/api/v3/repos/o/r/issues?since=x&per_page=100&page=2",
			want: "https://ghe.example.test/api/v3/repos/o/r/issues?page=2&per_page=100&since=x",
		},
		{
			name:    "relative",
			raw:     "/api/v3/repos/o/r/issues?page=2",
			wantErr: "must be absolute",
		},
		{
			name:    "cross origin",
			raw:     "https://attacker.example/api/v3/repos/o/r/issues?page=2",
			wantErr: "origin",
		},
		{
			name:    "wrong endpoint",
			raw:     "https://ghe.example.test/api/v3/user",
			wantErr: "path",
		},
		{
			name:    "userinfo",
			raw:     "https://user@ghe.example.test/api/v3/repos/o/r/issues?page=2",
			wantErr: "userinfo",
		},
		{
			name:    "fragment",
			raw:     "https://ghe.example.test/api/v3/repos/o/r/issues?page=2#fragment",
			wantErr: "fragment",
		},
		{
			name:    "cycle with reordered query",
			raw:     "https://ghe.example.test/api/v3/repos/o/r/issues?since=x&per_page=100&page=2",
			seen:    seenCycle,
			wantErr: "cycle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateGitHubNextURL(initial, tt.raw, tt.seen)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateGitHubNextURL: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
```

Replace the test file import block with:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)
```

- [ ] **Step 3: Run the parser and validator tests and confirm the red state**

Run:

```sh
GO111MODULE=on go test ./internal/eventbus -run 'TestGitHubNextLink|TestValidateGitHubNextURL' -count=1
```

Expected: compilation fails because the three private helpers do not exist.

- [ ] **Step 4: Implement strict Link parsing**

Add this code above `GitHubSource.listIssues` in
`internal/eventbus/source_github.go`:

```go
func githubNextLink(header string) (string, bool, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false, nil
	}

	var next string
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		end := strings.IndexByte(part, '>')
		if !strings.HasPrefix(part, "<") || end < 2 {
			return "", false, fmt.Errorf("malformed GitHub Link value %q", part)
		}

		target := part[1:end]
		if _, err := url.Parse(target); err != nil {
			return "", false, fmt.Errorf("malformed GitHub Link target %q: %w", target, err)
		}

		parameters := strings.TrimSpace(part[end+1:])
		if parameters != "" && !strings.HasPrefix(parameters, ";") {
			return "", false, fmt.Errorf("malformed GitHub Link parameters %q", parameters)
		}
		isNext := false
		for _, parameter := range strings.Split(strings.TrimPrefix(parameters, ";"), ";") {
			parameter = strings.TrimSpace(parameter)
			if parameter == "" {
				continue
			}
			key, value, ok := strings.Cut(parameter, "=")
			if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
				return "", false, fmt.Errorf("malformed GitHub Link parameter %q", parameter)
			}
			if !strings.EqualFold(strings.TrimSpace(key), "rel") {
				continue
			}
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, `"`) != strings.HasSuffix(value, `"`) {
				return "", false, fmt.Errorf("malformed GitHub Link rel value %q", value)
			}
			relations := strings.Fields(strings.Trim(value, `"`))
			for _, relation := range relations {
				if strings.EqualFold(relation, "next") {
					isNext = true
					break
				}
			}
		}
		if !isNext {
			continue
		}
		if next != "" {
			return "", false, fmt.Errorf("multiple rel=next values in GitHub Link header")
		}
		next = target
	}

	return next, next != "", nil
}
```

- [ ] **Step 5: Implement canonicalization and trust validation**

Add this code immediately after `githubNextLink`:

```go
func canonicalGitHubURL(u *url.URL) string {
	canonical := *u
	canonical.Scheme = strings.ToLower(canonical.Scheme)
	canonical.Host = strings.ToLower(canonical.Host)
	canonical.RawQuery = canonical.Query().Encode()
	return canonical.String()
}

func validateGitHubNextURL(
	initialURL string,
	rawNext string,
	seen map[string]struct{},
) (string, error) {
	initial, err := url.Parse(initialURL)
	if err != nil {
		return "", fmt.Errorf("parse initial GitHub issues URL: %w", err)
	}
	next, err := url.Parse(rawNext)
	if err != nil {
		return "", fmt.Errorf("parse GitHub next link: %w", err)
	}
	if !next.IsAbs() {
		return "", fmt.Errorf("GitHub next link must be absolute")
	}
	if next.User != nil {
		return "", fmt.Errorf("GitHub next link contains userinfo")
	}
	if next.Fragment != "" {
		return "", fmt.Errorf("GitHub next link contains fragment")
	}
	if !strings.EqualFold(next.Scheme, initial.Scheme) ||
		!strings.EqualFold(next.Host, initial.Host) {
		return "", fmt.Errorf("GitHub next link origin does not match initial request")
	}
	if next.Path != initial.Path {
		return "", fmt.Errorf(
			"GitHub next link path %q does not match %q",
			next.Path,
			initial.Path,
		)
	}

	canonical := canonicalGitHubURL(next)
	if _, ok := seen[canonical]; ok {
		return "", fmt.Errorf("GitHub next link cycle at %s", canonical)
	}
	return canonical, nil
}
```

- [ ] **Step 6: Run focused tests and the GitHub source regression set**

Run:

```sh
GO111MODULE=on go test ./internal/eventbus -run 'TestGitHubNextLink|TestValidateGitHubNextURL|TestGitHubSource' -count=1
```

Expected: all selected tests pass.

- [ ] **Step 7: Commit the Link safety primitives**

```sh
git add internal/eventbus/source_github.go internal/eventbus/source_github_test.go
git commit -m "eventbus: validate GitHub pagination links"
```

---

### Task 3: Traverse the complete bounded poll window

**Files:**
- Modify: `internal/eventbus/source_github_test.go`
- Modify: `internal/eventbus/source_github.go:454-465`

**Interfaces:**
- Consumes: `getJSONURL`, `githubNextLink`, `validateGitHubNextURL`,
  `canonicalGitHubURL`.
- Produces: `listIssues` returns the full successful window or `nil, error`.
- Constants:

```go
const (
	githubIssuesPerPage = 100
	githubIssuesMaxPages = 10
)
```

- [ ] **Step 1: Add a reusable issue fixture**

Add this helper near `newGHSource` in
`internal/eventbus/source_github_test.go`:

```go
func githubIssueFixtures(start, count int) []map[string]any {
	out := make([]map[string]any, 0, count)
	for n := start; n < start+count; n++ {
		out = append(out, map[string]any{
			"number":     n,
			"title":      fmt.Sprintf("issue-%d", n),
			"state":      "open",
			"user":       map[string]any{"login": "wu8685"},
			"comments":   0,
			"labels":     []map[string]any{{"name": "agent-build"}},
			"updated_at": "2026-07-03T10:00:00Z",
		})
	}
	return out
}
```

- [ ] **Step 2: Add the failing two-page success test**

Append:

```go
func TestGitHubSource_PaginatesRepositoryIssues(t *testing.T) {
	var srv *httptest.Server
	var requestedPages []string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
			return
		}
		if r.URL.Path != "/repos/o/r/issues" {
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		requestedPages = append(requestedPages, page)
		switch page {
		case "":
			if got := r.URL.Query().Get("per_page"); got != "100" {
				t.Errorf("per_page = %q, want 100", got)
			}
			next := *r.URL
			q := next.Query()
			q.Set("page", "2")
			next.RawQuery = q.Encode()
			w.Header().Set("Link", fmt.Sprintf("<%s%s>; rel=\"next\"", srv.URL, next.RequestURI()))
			_ = json.NewEncoder(w).Encode(githubIssueFixtures(1, 100))
		case "2":
			_ = json.NewEncoder(w).Encode(githubIssueFixtures(101, 1))
		default:
			t.Fatalf("unexpected page %q", page)
		}
	}))
	defer srv.Close()

	events, err := newGHSource(srv.URL).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(events) != 101 {
		t.Fatalf("events = %d, want 101", len(events))
	}
	if got := strings.Join(requestedPages, ","); got != ",2" {
		t.Fatalf("requested pages = %q, want \",2\"", got)
	}
}
```

- [ ] **Step 3: Run the success test and confirm the red state**

Run:

```sh
GO111MODULE=on go test ./internal/eventbus -run TestGitHubSource_PaginatesRepositoryIssues -count=1
```

Expected: fails because the first request still uses `per_page=50` and only one
page is decoded.

- [ ] **Step 4: Add failure-atomicity and page-budget tests**

Append:

```go
func TestGitHubSource_PageFailurePreservesSince(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
			return
		}
		if r.URL.Query().Get("page") == "2" {
			http.Error(w, "proxy failure", http.StatusInternalServerError)
			return
		}
		next := *r.URL
		q := next.Query()
		q.Set("page", "2")
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf("<%s%s>; rel=\"next\"", srv.URL, next.RequestURI()))
		_ = json.NewEncoder(w).Encode(githubIssueFixtures(1, 1))
	}))
	defer srv.Close()

	before := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	s := newGHSource(srv.URL)
	s.since = before
	if _, err := s.Fetch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "page 2") {
		t.Fatalf("Fetch err = %v, want page 2 error", err)
	}
	if !s.since.Equal(before) {
		t.Fatalf("since = %s, want unchanged %s", s.since, before)
	}
}

func TestGitHubSource_PageBudgetPreservesSince(t *testing.T) {
	var srv *httptest.Server
	requests := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
			return
		}
		requests++
		page := r.URL.Query().Get("page")
		pageNumber := 1
		if page != "" {
			pageNumber, _ = strconv.Atoi(page)
		}
		next := *r.URL
		q := next.Query()
		q.Set("page", strconv.Itoa(pageNumber+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf("<%s%s>; rel=\"next\"", srv.URL, next.RequestURI()))
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()

	before := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	s := newGHSource(srv.URL)
	s.since = before
	if _, err := s.Fetch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "10-page budget") {
		t.Fatalf("Fetch err = %v, want 10-page budget error", err)
	}
	if requests != 10 {
		t.Fatalf("issue requests = %d, want 10", requests)
	}
	if !s.since.Equal(before) {
		t.Fatalf("since = %s, want unchanged %s", s.since, before)
	}
}
```

- [ ] **Step 5: Add end-to-end Link rejection tests**

Replace the test file import block with:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)
```

Then append:

```go
func TestGitHubSource_RejectsCrossOriginNextBeforeRequest(t *testing.T) {
	var hostileRequests atomic.Int32
	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostileRequests.Add(1)
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer hostile.Close()

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
			return
		}
		w.Header().Set(
			"Link",
			fmt.Sprintf("<%s/repos/o/r/issues?page=2>; rel=\"next\"", hostile.URL),
		)
		_ = json.NewEncoder(w).Encode(githubIssueFixtures(1, 1))
	}))
	defer srv.Close()

	if _, err := newGHSource(srv.URL).Fetch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "origin") {
		t.Fatalf("Fetch err = %v, want origin error", err)
	}
	if got := hostileRequests.Load(); got != 0 {
		t.Fatalf("hostile requests = %d, want 0", got)
	}
}

func TestGitHubSource_RejectsWrongEndpointNextBeforeRequest(t *testing.T) {
	var wrongEndpointRequests atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
		case "/repos/o/r/issues":
			w.Header().Set(
				"Link",
				fmt.Sprintf("<%s/repos/o/r/pulls?page=2>; rel=\"next\"", srv.URL),
			)
			_ = json.NewEncoder(w).Encode(githubIssueFixtures(1, 1))
		case "/repos/o/r/pulls":
			wrongEndpointRequests.Add(1)
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if _, err := newGHSource(srv.URL).Fetch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "path") {
		t.Fatalf("Fetch err = %v, want path error", err)
	}
	if got := wrongEndpointRequests.Load(); got != 0 {
		t.Fatalf("wrong endpoint requests = %d, want 0", got)
	}
}

func TestGitHubSource_RejectsNextLinkCycle(t *testing.T) {
	var srv *httptest.Server
	var firstURL string
	issueRequests := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
			return
		}
		issueRequests++
		if firstURL == "" {
			firstURL = srv.URL + r.URL.RequestURI()
			next := *r.URL
			q := next.Query()
			q.Set("page", "2")
			next.RawQuery = q.Encode()
			w.Header().Set("Link", fmt.Sprintf("<%s%s>; rel=\"next\"", srv.URL, next.RequestURI()))
		} else {
			w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"next\"", firstURL))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()

	if _, err := newGHSource(srv.URL).Fetch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Fetch err = %v, want cycle error", err)
	}
	if issueRequests != 2 {
		t.Fatalf("issue requests = %d, want 2", issueRequests)
	}
}

func TestGitHubSource_MalformedLinkPreservesSince(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
			return
		}
		w.Header().Set("Link", "not-a-link")
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()

	before := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	s := newGHSource(srv.URL)
	s.since = before
	if _, err := s.Fetch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "malformed") {
		t.Fatalf("Fetch err = %v, want malformed Link error", err)
	}
	if !s.since.Equal(before) {
		t.Fatalf("since = %s, want unchanged %s", s.since, before)
	}
}
```

- [ ] **Step 6: Add item-detail failure and terminal-page boundary tests**

Append:

```go
func TestGitHubSource_ItemDetailFailurePreservesSince(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
		case "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":       1,
				"title":        "PR",
				"state":        "open",
				"user":         map[string]any{"login": "wu8685"},
				"comments":     0,
				"labels":       []map[string]any{{"name": "agent-build"}},
				"updated_at":   "2026-07-03T10:00:00Z",
				"pull_request": map[string]any{"url": "present"},
			}})
		case "/repos/o/r/pulls/1":
			http.Error(w, "detail failure", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	before := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	s := newGHSource(srv.URL)
	s.since = before
	if _, err := s.Fetch(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "pr 1 detail") {
		t.Fatalf("Fetch err = %v, want pr 1 detail error", err)
	}
	if !s.since.Equal(before) {
		t.Fatalf("since = %s, want unchanged %s", s.since, before)
	}
}

func TestGitHubSource_TerminalPageBoundaries(t *testing.T) {
	for _, count := range []int{0, 100} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			issueRequests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					_ = json.NewEncoder(w).Encode(map[string]any{"login": "wu8685"})
					return
				}
				issueRequests++
				w.Header().Set(
					"Link",
					`<https://api.example.test/repos/o/r/issues?page=1>; rel="prev"`,
				)
				_ = json.NewEncoder(w).Encode(githubIssueFixtures(1, count))
			}))
			defer srv.Close()

			events, err := newGHSource(srv.URL).Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if len(events) != count {
				t.Fatalf("events = %d, want %d", len(events), count)
			}
			if issueRequests != 1 {
				t.Fatalf("issue requests = %d, want 1", issueRequests)
			}
		})
	}
}
```

- [ ] **Step 7: Run all new traversal tests and confirm the red state**

Run:

```sh
GO111MODULE=on go test ./internal/eventbus -run 'TestGitHubSource_(Paginates|PageFailure|PageBudget|Rejects|MalformedLink|ItemDetailFailure|TerminalPage)' -count=1
```

Expected: at least the success, budget, and failure-path tests fail because
`listIssues` still performs one `per_page=50` request.

- [ ] **Step 8: Implement the bounded pagination loop**

Add the constants near the GitHub source type declarations:

```go
const (
	githubIssuesPerPage  = 100
	githubIssuesMaxPages = 10
)
```

Replace `listIssues` with:

```go
func (s *GitHubSource) listIssues(ctx context.Context, since time.Time) ([]ghIssue, error) {
	q := url.Values{}
	q.Set("state", s.state())
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	q.Set("per_page", strconv.Itoa(githubIssuesPerPage))
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}

	initialURL := s.apiBase() + "/repos/" + s.Repo + "/issues?" + q.Encode()
	initial, err := url.Parse(initialURL)
	if err != nil {
		return nil, fmt.Errorf("parse initial GitHub issues URL: %w", err)
	}
	currentURL := canonicalGitHubURL(initial)
	seen := map[string]struct{}{currentURL: {}}
	var issues []ghIssue

	for page := 1; page <= githubIssuesMaxPages; page++ {
		var batch []ghIssue
		headers, err := s.getJSONURL(ctx, currentURL, &batch)
		if err != nil {
			return nil, fmt.Errorf("GitHub issues page %d: %w", page, err)
		}
		issues = append(issues, batch...)

		rawNext, ok, err := githubNextLink(headers.Get("Link"))
		if err != nil {
			return nil, fmt.Errorf("GitHub issues page %d Link: %w", page, err)
		}
		if !ok {
			return issues, nil
		}
		if page == githubIssuesMaxPages {
			return nil, fmt.Errorf(
				"GitHub issues pagination exceeds %d-page budget",
				githubIssuesMaxPages,
			)
		}

		nextURL, err := validateGitHubNextURL(initialURL, rawNext, seen)
		if err != nil {
			return nil, fmt.Errorf("GitHub issues page %d Link: %w", page, err)
		}
		seen[nextURL] = struct{}{}
		currentURL = nextURL
	}

	return nil, fmt.Errorf("GitHub issues pagination ended without a terminal page")
}
```

- [ ] **Step 9: Run traversal, GitHub regression, and package tests**

Run:

```sh
GO111MODULE=on go test ./internal/eventbus -run 'TestGitHubSource|TestGitHubNextLink|TestValidateGitHubNextURL' -count=1
GO111MODULE=on go test ./internal/eventbus -count=1
```

Expected: all tests pass.

- [ ] **Step 10: Commit lossless traversal**

```sh
git add internal/eventbus/source_github.go internal/eventbus/source_github_test.go
git commit -m "eventbus: paginate GitHub issue polling"
```

---

### Task 4: Correct source documentation and run the full verification gate

**Files:**
- Modify: `docs/EVENTBUS-SOURCES.md`
- Modify: `docs/ROADMAP.md`

**Interfaces:**
- Documents the behavior implemented by Tasks 1–3.
- Does not change a Go API or persisted configuration.

- [ ] **Step 1: Document the pagination and failure contract**

In `docs/EVENTBUS-SOURCES.md`, insert this paragraph immediately before
`**No boot replay.**` in the `github` source section:

```markdown
**Bounded lossless pagination.** Each poll requests repository issues with
`per_page=100` and follows GitHub's same-origin, same-endpoint `Link rel="next"`
chain before advancing the in-memory `since` watermark. The first page counts
toward a fixed 10-page limit. A page error, malformed/unsafe/cyclic next link, or
a next link after page 10 fails the whole fetch: the Poller dispatches nothing
and the unchanged watermark is retried on the next interval. This protects one
running poll window; it does not backfill activity across a process restart.
```

- [ ] **Step 2: Replace the inaccurate ROADMAP note**

Replace the existing `Eventbus reliability` bullet in `docs/ROADMAP.md` with:

```markdown
- **Eventbus reliability** — a transient GitHub fetch failure is retried on the
  next poll interval without advancing `since`; repository-issues pagination is
  exhausted before a successful advance, with a fixed 10-page safety cap. The
  remaining gap is restart recovery: the in-memory watermark resets to `now`, so
  activity during downtime is not backfilled. The event log can reconcile only
  events that were already ingested.
```

- [ ] **Step 3: Verify the documentation matches the implementation**

Run:

```sh
rg -n "per_page=100|10-page|restart|without advancing" \
  docs/EVENTBUS-SOURCES.md docs/ROADMAP.md
git diff --check
```

Expected: both documents describe the fixed page cap, no-advance failure
semantics, and the separate restart limitation; `git diff --check` is silent.

- [ ] **Step 4: Run the complete repository verification**

Run:

```sh
GO111MODULE=on go test ./internal/eventbus -count=1
GO111MODULE=on go test -short ./... -count=1
GO111MODULE=on go vet ./...
GO111MODULE=on go build ./...
git diff --check
```

Expected: every command exits 0.

- [ ] **Step 5: Perform an independent review**

Give the reviewer:

- `docs/superpowers/specs/2026-07-23-github-source-pagination-design.md`
- this implementation plan;
- the complete diff from the pre-implementation commit;
- red and green test output for Tasks 1–3;
- full verification output from Step 4.

The reviewer must check:

1. pagination remains in `GitHubSource`;
2. no token can cross origin or endpoint;
3. page 10 cannot lead to request 11;
4. every pagination failure preserves `since`;
5. existing event/trust semantics are unchanged;
6. restart recovery is not falsely claimed.

Expected verdict: `PASS`, or specific `NEEDS_WORK` findings followed by another
test-first correction and re-review.

- [ ] **Step 6: Commit documentation after review passes**

```sh
git add docs/EVENTBUS-SOURCES.md docs/ROADMAP.md
git commit -m "docs: define GitHub pagination reliability"
```

- [ ] **Step 7: Confirm final repository state**

Run:

```sh
git status --short
git log -4 --oneline
```

Expected: clean worktree and four focused commits for helper refactor, Link
safety, traversal, and documentation.

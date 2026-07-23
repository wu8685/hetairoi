# GitHub Source Lossless Pagination Design

> Status: approved 2026-07-24
> Target: `internal/eventbus` in Hetairoi
> Development method: SDD → user approval → TDD → minimal implementation

## 1. Problem

`GitHubSource.listIssues` currently requests one repository-issues page with
`per_page=50`, ignores the response `Link` header, and then allows `Fetch` to
advance `since` after processing that page.

If more than 50 issues or pull requests are updated in one poll window, items
outside the first page can be omitted without an error. Handler dedup and native
event retry cannot recover them because they never entered the event bus.

## 2. Ownership boundary

Pagination belongs to the built-in `GitHubSource`, not to an event handler:

```text
GitHub REST API
  → GitHubSource: authentication, pagination, since window, typed events
  → Poller / Bus: interval scheduling, dispatch-on-success, Event.ID dedup
  → Handler: match events, select session policy, dispatch an agent
```

The source is the only component that can see GitHub's `Link` header and owns
the `since` watermark. A handler receives events after ingestion and therefore
cannot detect or recover a page that the source never read.

The generic `Poller` remains upstream-agnostic. GitHub-specific page traversal
must not leak into the bus or handler model.

## 3. Goals

1. Read every repository-issues page in one logical poll window before allowing
   `since` to advance.
2. Treat page traversal as atomic from the Poller's perspective: a page failure
   returns an error, dispatches no events, and preserves the previous `since`.
3. Bound work to 10 pages per poll, including the first page.
4. Reject unsafe, malformed, or cyclic next links before following them.
5. Preserve the existing GitHub event types, owner trust boundary, routing
   payloads, dedup IDs, restart behavior, and SourceSpec schema.

With `per_page=100`, one successful poll can ingest at most 1,000 list items.

## 4. Non-goals

- Persisting `since` across process restarts.
- Backfilling activity that occurred while Hetairoi was stopped.
- Adding overlap windows or reconciliation passes.
- Guaranteeing exactly-once delivery.
- Eliminating gaps caused by mutable `updated_at` ordering during traversal.
- Adding a configurable `max_pages` field to `SourceSpec`.
- Building a generic paginator for other upstreams.
- Moving the built-in GitHub source into an `exec` source plugin.

These concerns require separate evidence and design decisions.

## 5. Required behavior

### 5.1 Initial request

The first repository-issues request must retain the existing parameters:

- `state=<configured state>`
- `sort=updated`
- `direction=desc`
- `since=<previous successful watermark>`, when non-zero

It must change `per_page` from 50 to 100.

### 5.2 Page traversal

After decoding a page, the source must inspect the response `Link` header:

1. If no `rel="next"` link exists, traversal is complete.
2. If a next link exists and fewer than 10 pages have been requested, validate
   the link and request it.
3. Append decoded items in page order.
4. Repeat until no next link remains.
5. If page 10 still advertises a next link, return a page-budget error without
   requesting page 11.

Partial page results must not be returned as a successful list.

### 5.3 Next-link validation

Before a next link is requested, it must be parsed as an absolute URL and pass
all of these checks:

- scheme equals the scheme of the initial repository-issues URL;
- host, including an explicit port, equals the initial URL host;
- userinfo is absent;
- fragment is absent;
- path exactly equals the initial repository-issues endpoint path, including
  any path prefix in a GitHub Enterprise `api_base`;
- the resolved absolute URL has not already been requested in this traversal.

The query string may differ because GitHub controls pagination parameters. The
source follows the validated URL rather than reconstructing `page=N`.

A non-empty `Link` header that cannot be safely parsed must return an error.
Unknown well-formed relations such as `prev`, `first`, or `last` are ignored.

### 5.4 HTTP helper boundary

Authentication, GitHub media headers, API version headers, client selection,
status handling, bounded error-body reading, and JSON decoding must continue to
have one implementation.

The design introduces a private full-URL helper with this contract:

```go
func (s *GitHubSource) getJSONURL(
    ctx context.Context,
    rawURL string,
    v any,
) (http.Header, error)
```

It sends one authenticated GET, decodes `v`, and returns response headers.

The existing relative-path helper remains available to every current caller:

```go
func (s *GitHubSource) getJSON(
    ctx context.Context,
    path string,
    v any,
) error
```

It delegates to `getJSONURL(s.apiBase()+path, v)` and discards the returned
headers. This avoids duplicating request or error logic inside `listIssues`.

`listIssues` constructs its initial absolute URL, calls `getJSONURL`, and follows
only validated absolute next links.

### 5.5 Watermark and dispatch semantics

`Fetch` captures `tickStart` before listing, as it does today.

- Complete traversal and successful item-detail processing: return events and
  set `since = tickStart`.
- Any list page, Link parse, Link validation, page-budget, or item-detail
  failure: return a non-nil error and do not change `since`.
- `Poller.tick` must continue to dispatch nothing whenever `Fetch` returns an
  error, even if the event slice is non-empty.

The next interval retries from the unchanged watermark. Existing per-handler
`Event.ID` dedup absorbs repeat events produced by the overlapping retry.

## 6. Error and observability contract

Pagination errors must identify the source operation and page number without
including the Authorization header or token. Expected error categories:

- page request or JSON decode failure;
- malformed `Link` header;
- cross-origin next link;
- unexpected next-link endpoint path;
- cyclic next link;
- 10-page budget exceeded.

Existing Poller logging is sufficient for this change; no metric, retry loop, or
new control-plane field is required.

## 7. Security properties

GitHub response metadata is still untrusted input.

- A next link is validated before the authenticated client follows it, so the
  bearer token cannot be forwarded to another origin.
- Exact endpoint-path validation prevents the pagination loop from turning into
  an authenticated arbitrary GitHub API reader.
- Cycle detection and the 10-page cap bound hostile or corrupted Link behavior.
- Existing owner-only issue content and review-verdict checks remain unchanged.

## 8. TDD acceptance scenarios

Tests belong in `internal/eventbus/source_github_test.go` and use
`httptest.Server`. Each behavior must be demonstrated red before implementation.

1. **Two-page success**
   - Page 1 returns 100 issues and a same-origin `rel="next"` link.
   - Page 2 returns one issue and no next link.
   - `Fetch` returns 101 issue events.
   - The first request contains `per_page=100`; the supplied next URL is followed.

2. **Second-page HTTP failure**
   - Page 1 returns a valid next link; page 2 returns 500.
   - `Fetch` returns an error.
   - `since` equals its value before the call.

3. **Page-budget exhaustion**
   - Pages 1 through 10 each return a unique next link.
   - Page 10 still advertises page 11.
   - The source returns an error and never requests page 11.
   - `since` remains unchanged.

4. **Cycle detection**
   - A later page links to an already requested URL.
   - The source returns an error before issuing the repeated request.

5. **Cross-origin rejection**
   - The next link points to a second test server.
   - The source returns an error.
   - The second server records zero requests.

6. **Wrong endpoint rejection**
   - The next link keeps the origin but changes the path away from the repository
     issues endpoint.
   - The source returns an error before following it.

7. **Malformed Link rejection**
   - A non-empty malformed Link header cannot be safely parsed.
   - The source returns an error and preserves `since`.

8. **Terminal-page boundaries**
   - Empty first page with no Link succeeds.
   - Exactly 100 items with no Link succeeds.
   - A response with only well-formed non-next relations succeeds without an
     additional request.

9. **Existing regression suite**
   - Issue, `pr.push`, and `pr.review` event construction remains unchanged.
   - Owner authorization, non-owner rejection, marker guards, kind filters,
     comment-page selection, and dedup tests continue to pass.

## 9. Files in the implementation scope

- Modify `internal/eventbus/source_github.go`:
  page constants, Link parsing/validation, header-aware GET helper, and the
  `listIssues` traversal.
- Modify `internal/eventbus/source_github_test.go`:
  pagination and failure fixtures plus acceptance tests.
- Modify `docs/EVENTBUS-SOURCES.md`:
  document complete-page traversal, the 10-page cap, and failure semantics.
- Modify `docs/ROADMAP.md`:
  replace the inaccurate “failed poll has no retry” note with the actual
  pagination and restart limitations.

No other production package, API route, persisted schema, deployment file, or
external repository is in scope.

## 10. Verification

Implementation is acceptable only when all commands succeed:

```sh
GO111MODULE=on go test ./internal/eventbus
GO111MODULE=on go test -short ./...
GO111MODULE=on go vet ./...
GO111MODULE=on go build ./...
```

The implementation must also pass `git diff --check` and an independent review
of the spec, diff, tests, and failure evidence.

## 11. Rollback boundary

The change can be reverted entirely within the four files listed above. If the
page cap causes unacceptable rate-limit or latency behavior, rollback must
restore the previous code while documenting that the built-in GitHub source is
best-effort and may omit updates beyond the first page. Silent truncation must
not be described as complete ingestion.

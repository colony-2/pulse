# Response: public Go job-listing API

## Outcome

Implemented the request in `C2J_FEATURE_REQUESTS.md` as the supported public
package `github.com/colony-2/c2j/pkg/joblist`.

Implementation, shared CLI integration, example, and tests: commit `31ceb43`.

A Go application can now supply a remote JobDB URI and an explicit repository
identity and receive typed, paginated jobs with c2j's execution interpretation.
It does not invoke the c2j executable, discover local configuration, initialize
an executor or embedded runtime, or mutate job state. No JobDB changes or
dependency version updates were needed.

The CLI uses the same query builder, execution filtering, and result projection.
Existing `pkg/recipejob` helpers remain available and share the execution/status
logic. List JSON gains the optional `repo` field; existing fields and execution
semantics are retained.

## Using it

```go
import (
    "context"
    "time"

    "github.com/colony-2/c2j/pkg/joblist"
    "github.com/colony-2/jobdb/pkg/jobdb"
)

func waitingJobs(ctx context.Context) (joblist.Page, error) {
    client, err := joblist.New(joblist.Config{
        JobDBURI: "https://jobs.example.com/team-a",
    })
    if err != nil {
        return joblist.Page{}, err
    }
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()
    return client.List(ctx, joblist.Query{
        Repository: "github.com/acme/app",
        JobTypes:   []string{"recipe"},
        Statuses: []jobdb.JobStatus{
            jobdb.JobStatusReady,
            jobdb.JobStatusCrashConcern,
        },
        PageSize: 50,
    })
}
```

The URI's path selects tenant `team-a`; it is not an HTTP API base path.
`Repository` uses existing c2j repository normalization and exact matching.
Configured short names and `--self` are not discovered: pass the cell's explicit
repository identity. Fully explicit inputs ignore unrelated or invalid local
project configuration.

Reuse one client concurrently for the same connection/tenant, passing a
different repository in each query as needed. Use separate clients for different
tenants. There is no client `Close`; custom HTTP transports remain caller-owned.

For authentication, custom TLS, or timeout policies, provide a `*http.Client`
in `Config.HTTPClient`. The [API guide](pkg/joblist/README.md) includes an
authorization-transport example and explains error handling and lifecycle.

### Pagination

Each call returns one logical page. Copy `page.NextPageToken` to
`query.PageToken` for the next call, leaving the rest of the query unchanged.
Stop only when the token is empty, even if an intermediate page has no jobs.
Tokens are opaque backend cursors, not transactional snapshots.

Without execution filtering, a call performs one backend page request. With
`query.ExecutionFilter`, it may scan multiple backend pages to fill the logical
page. Page size bounds returned jobs, not total scan work; context deadlines
bound the duration of remote work. The guide documents defaults, backend caps,
cursor lifetime limitations, and sparse-match behavior.

### Execution information

Read `job.Execution` instead of interpreting metadata or raw client payloads.
For a usable resolved demand, `job.Execution.Demand.Effective` contains the
effective image/platform/resources. The demand also carries schema and revision
information, and the view reports its source.

- `specified`: resolved requirements are present.
- `unspecified`: resolved, with no requirements; application defaults may apply.
- `unresolved`: not fully known; do not treat as an empty resolved demand.
- `malformed` / `unsupported`: inspect `Diagnostic`; do not silently default.
- `in_flight` / `not_waiting`: requirements are unavailable, not a stale recorded
  snapshot presented as current.

Only waiting jobs expose requirements. `ACTIVE` jobs may hold newer ephemeral
requirements, and terminal jobs do not advertise scheduling demand.

In ordinary listing, unusable execution data is an item diagnostic. When using
compatibility filtering, it fails the page with `*joblist.ExecutionDemandError`,
which identifies the affected job. Context cancellation/deadline errors remain
inspectable with `errors.Is`; invalid configuration/query inputs wrap
`joblist.ErrInvalidInput`.

## Standalone example and packaging

The [complete example](examples/listjobs/main.go) covers connection setup,
one-page listing, opt-in pagination, execution-view access, interruption,
deadlines, and errors:

```bash
go run ./examples/listjobs \
  -jobdb https://jobs.example.com/team-a \
  -repo github.com/acme/app \
  -page-size 50
```

Add `-all` to iterate pages explicitly. It emits one JSON object per page;
without `-all`, it emits just one page. The JSON output is only this example's
presentation choice; library callers receive Go values directly.

Copy the example into your application module and pin a c2j release containing
this change. Build the application's single executable, for example:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o listjobs .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o listjobs-arm64 .
```

No c2j executable, Node/npm, Git, shell, checkout, or executor toolchain is
required at runtime. HTTPS still requires suitable CA trust and normal network
configuration. The supported entry points and v0 compatibility/migration policy
are documented in the [API guide](pkg/joblist/README.md).

## Verification

Validated with Go 1.26.1:

- Full repository tests: `go test ./...` and the CI-equivalent
  `go test -tags=integration ./...`.
- Focused race tests for `pkg/joblist`, recipe-job listing, CLI listing/defaults,
  and shared internal helpers.
- `go build ./...`, `go vet ./...`, and `go mod tidy -diff`.
- CLI/public API query and result parity for `READY`, `CRASH_CONCERN`, `ACTIVE`,
  and terminal jobs, including typed routes and execution views.
- Concurrent tenant/repository isolation against the actual JobDB toy backend
  through its HTTP server; pagination, empty results, and invalid tokens.
- Recorded demand overrides/revisions, missing/resolved-empty demand, invalid
  data/schema diagnostics, and waiting-only visibility.
- Sparse compatibility matches, empty intermediate pages, bounded returned
  results, and non-advancing cursor rejection.
- Authentication failures/custom authentication, connection errors, cancellation,
  deadlines, invalid input, and independence from local configuration and PATH.
- A separate temporary Go module builds the copied example with CGO disabled
  for Linux AMD64 and ARM64. Both binaries ran successfully in this environment
  against a test server with an empty working directory and tool-free PATH.
  ELF checks verify no dynamic-linker dependency. Dependency checks exclude
  c2j worker/config/starter packages, embedded SQLite, and Docker tooling.

Read-only tests expose only the backend's `ListJobs` operation after fixture
setup. No schema registration, claims, execution, lease renewal, or state writes
are needed to construct or use the client.

The standalone test also runs under the existing CI test matrix. On hosts
without the other architecture's execution support, it verifies that target's
static build and executes the matching architecture; the Linux AMD64 and ARM64
CI jobs provide native coverage for their respective targets.

## Release availability

Implementation and local consumer verification are complete. The external-module
test uses a local `replace` to exercise this not-yet-published source; it does
not claim that an older published release already contains the API. A released
version becomes available through the repository's normal release process after
these commits are pushed and released. This work does not publish or push a
release. Downstream users should pin that resulting version and remove any
development-only replacement.

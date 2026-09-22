# Feature request: a supported public Go API for job listing

## Summary

Expose a documented, supported public Go API for listing c2j jobs and their execution requirements. A Go application should be able to obtain the information available through `c2j list --json` without installing or invoking the c2j executable, parsing CLI output, importing CLI-internal packages, or independently reconstructing c2j's JobDB queries and execution metadata interpretation.

This request is limited to read-only listing. Existing job execution, runtime requirement changes, conditional yielding, and recovery semantics already satisfy the underlying requirements.

## Motivation

Services that discover jobs need c2j's interpretation of those jobs, including repository scope, routes, and effective execution requirements. A public library interface would let such services ship a single Go executable and use normal Go types, errors, and cancellation instead of a subprocess and JSON boundary.

For explicitly configured remote listing, the application must be able to run in a minimal container without a separate c2j binary, Node.js/npm, a shell, Git, or a local repository checkout. The immediate packaging benefit is removing the additional c2j executable from the listing application's image; a distroless deployment already avoids Node.js and a shell.

The API should serve any Go application that lists c2j jobs. It should have no dependency on a particular scheduler, compute provider, or runner service.

## What exists today

Reviewed c2j commit [`5a2b646395a29ed80af4a08935fef1095838564f`](https://github.com/colony-2/c2j/commit/5a2b646395a29ed80af4a08935fef1095838564f).

Public functionality already includes:

- [`pkg/recipejob` listing helpers and result types](https://github.com/colony-2/c2j/blob/5a2b646395a29ed80af4a08935fef1095838564f/pkg/recipejob/list.go), including `ListRecipeJobs` and `RecipeJob`.
- [`ListExecutionJobs` and `ExecutionView`](https://github.com/colony-2/c2j/blob/5a2b646395a29ed80af4a08935fef1095838564f/pkg/recipejob/execution.go).
- [Repository/cell target resolution](https://github.com/colony-2/c2j/blob/5a2b646395a29ed80af4a08935fef1095838564f/pkg/recipejob/target.go).

The CLI's [listing orchestration and output projection](https://github.com/colony-2/c2j/blob/5a2b646395a29ed80af4a08935fef1095838564f/cmd/c2j/internal/listjobs/service.go) and [runtime opening](https://github.com/colony-2/c2j/blob/5a2b646395a29ed80af4a08935fef1095838564f/cmd/c2j/internal/swfruntime/runtime.go) remain under `cmd/c2j/internal`.

The unmet need is a **supported public path from explicit connection/query inputs to typed listing results**, with documented compatibility and behavior. Existing helpers may meet parts of this requirement; no duplicate implementation or particular new package structure is requested.

## Required behavior

### 1. Explicit, isolated configuration

A caller must be able to supply:

- The remote JobDB connection target and tenant, with the same tenant-selection meaning as the corresponding c2j CLI options.
- An explicit repository/cell selector, using c2j's existing matching and normalization semantics.
- The connection/authentication configuration needed for supported remote access.

Explicit inputs must be sufficient. Listing a fully specified remote repository must not require Git, a checkout, current-directory discovery, or modification of process-wide environment variables. Different clients or calls in one process must be able to address different tenants and repositories without configuration leaking between them.

A caller must not need to construct JobDB repository metadata filters or wire up c2j's execution projection itself. This does not require hiding all existing public dependency types; it requires c2j to own and document the supported listing integration.

### 2. Bounded, paginated queries

Support the listing behavior needed by a polling service:

- Repository/cell and tenant scope.
- Job-type and status filters, including recipe jobs in `READY` and `CRASH_CONCERN`.
- A page-size limit and an opaque continuation token.
- A typed page of results and the continuation token, including empty-result behavior.

A caller must be able to fetch one page without automatically draining the entire result set. Document page limits, token lifetime or invalidation rules, and the requirement to keep filters consistent between pages. Preserve existing CLI/backend pagination semantics; a new transactional snapshot guarantee is not requested.

Full parity with every optional CLI filter is not required for the initial API. Any filters it exposes must have the same meaning as their CLI equivalents.

### 3. Typed job and execution information

Each result must expose the information needed to identify a job and decide whether to request an executor:

| Information | Required content |
| --- | --- |
| Identity and scope | Tenant ID, job ID, and the job's repository identity when present in its metadata. |
| Current state | Job type, status, availability time, cancellation-requested state, and next route, including job/task type. |
| Execution view | c2j's execution status, source, and diagnostics, including whether requirements are unavailable or unresolved. |
| Execution demand | Effective image, platform, CPU, memory, and ephemeral-storage requirements, with their schema/revision information where present. |

Preserve the distinction between an omitted requirement, an unresolved requirement, and an invalid requirement. A caller must be able to apply its own defaults to absent values without accidentally treating a diagnostic or unsupported schema as an empty/default request.

Effective requirements and execution-view interpretation must remain c2j's responsibility. Callers must not need to merge recipe and job requirements, inspect raw client payloads, or track c2j's internal metadata layout. Reusing existing public execution types is acceptable; a new unit system or execution-requirements model is not requested.

### 4. Read-only library behavior

- Accept `context.Context` and honor cancellation and deadlines for remote work.
- Return results and errors to the caller without printing CLI output, exiting the process, or mutating global configuration.
- Preserve inspectable cancellation/deadline errors and provide useful errors for invalid input, connection/authentication failures, and unusable execution information. Document whether a job-level diagnostic is returned on the item or fails the page, consistent with the corresponding listing mode.
- Do not claim jobs, renew leases, execute work, or publish changes in execution requirements as a side effect of listing.
- Document client lifecycle and concurrency behavior so a long-running service can reuse the interface safely.

The listing path must work as part of a statically built Go application on Linux AMD64 and ARM64. Importing and using it must not require installation or initialization of a command runner, recipe executor, local runtime, or external tools. Keep the listing dependency footprint appropriate for an application that only reads job information; no specific package or module split is mandated.

### 5. A formal compatibility contract

Publish documentation and a compilable example showing connection configuration, one-page listing, pagination, execution-view access, cancellation, and error handling.

Identify the public entry points and types intended for downstream use, and document how breaking changes are communicated across c2j releases. Callers should be able to pin a released Go module version. Existing public helpers may be designated as supported where appropriate; a new major module version is not required by this request.

CLI and library results must agree for equivalent queries against the same data. Differences limited to CLI formatting or documented optional fields are acceptable; execution semantics must agree.

## Acceptance criteria

1. An example application outside the c2j repository can depend on a released c2j module and list remote jobs using only documented public APIs.
2. With explicit connection, tenant, and repository inputs, it works without the c2j executable, Node.js/npm, a shell, Git, or a repository checkout.
3. Equivalent library and CLI queries agree on job identity, filtering, routing/state, pagination, and execution requirements. Tests cover both `READY` and `CRASH_CONCERN` jobs and isolation between tenants/repositories.
4. Results correctly distinguish specified, unspecified, unresolved, unavailable, and invalid execution information according to existing c2j semantics. Effective requirements reflect changes already recorded by c2j.
5. Pagination, empty pages, invalid inputs, remote errors, and context cancellation have documented, tested behavior.
6. Listing performs no job claims, execution, lease renewal, or job-state mutations.
7. A minimal Linux AMD64/ARM64 application using the API can run without the CLI or execution toolchain installed.

## Scope boundary

This request does not ask for a public job-execution API, an embedded CLI, runner registration, provider selection, notifications, capacity tracking, cooldown logic, or new job lifecycle semantics. Existing executor containers continue to run `c2j run` as usual.

Choosing this library by default while retaining an optional external-c2j listing adapter is a downstream application's integration choice. It does not require additional c2j functionality beyond this public listing API.

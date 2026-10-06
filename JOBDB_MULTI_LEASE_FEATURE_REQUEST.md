# Feature request: batch acquisition of selected job leases

Status: proposed. This request describes a generic JobDB capability for any client that selects jobs before acquiring execution leases.

## Summary

Add a supported public operation to acquire ordinary execution leases for an explicit list of jobs in one request. Each job is independently eligible or ineligible, and the response reports an outcome for each requested job.

This should be the batch equivalent of `GetJobLease`, preserving its authorization, route matching, readiness, ownership, and lease-duration semantics. It introduces no new lease state or ownership protocol.

## Existing capability and gap

Reviewed against JobDB v0.0.22, particularly `pkg/jobdb/runtime.go` and `pkg/jobdb/runtime/remote/runtime.go`.

The public runtime already provides:

- `GetJobLease(GetJobLeaseRequest)`: acquire a lease for one specified `JobKey`, with a worker identity, acceptable routes, and lease duration.
- `PollWork(PollWorkRequest)`: acquire multiple jobs selected by JobDB using tenant, routes, metadata predicates, and a limit.

`PollWork` already serves clients that want JobDB to select available work. The missing operation serves clients that have selected specific job identities and need to claim that set. Such selection can follow a listing, application policy, or a user action.

Today these clients issue a separate `GetJobLease` call for each job. Sequential requests accumulate latency; concurrent requests require every client to implement its own batching and concurrency control. A batch API reduces HTTP round trips and lets each backend choose efficient acquisition strategies while retaining independent ownership decisions.

## Proposed public shape

Names and exact types are illustrative; reuse existing public types where practical:

```go
type GetJobLeasesRequest struct {
    Items       []GetJobLeaseRequest
    StartWindow time.Duration // optional; limits starting new acquisition attempts
}

type GetJobLeasesResponse struct {
    Results []JobLeaseResult
}

type JobLeaseResult struct {
    JobKey JobKey
    Status JobLeaseStatus
    Lease  ExecutionLease // present only for acquired
    Code   string         // optional machine-readable failure category
}
```

Expose this through a supported Go API and the remote HTTP API, with equivalent behavior across supported backends. HTTP responses use the existing execution-lease representation; Go clients return usable lease objects. No particular endpoint path or interface migration strategy is required.

Each item retains the existing single-job request's worker identity, acceptable routes, and lease duration. Different jobs may use different values. Require one tenant per batch initially; separate tenants use separate calls. This keeps authorization and backend routing straightforward.

## Acquisition semantics

1. **Explicit identities only.** Attempt only the supplied jobs. Do not substitute another job when a requested job is unavailable, and do not turn this into a query or polling operation.
2. **Independent acquisition.** Apply the same atomic checks as `GetJobLease` for each job. A stale listing, changed route, existing owner, cancellation, or future availability must not be bypassed by batching.
3. **Partial success is normal.** Failure to acquire one job does not roll back successful acquisitions or prevent attempts for the remaining jobs within the request's bounds. The batch is not an all-or-nothing transaction or a consistent snapshot across jobs.
4. **Ordinary leases.** Each success returns its own lease identity, credential where applicable, authoritative execution state, client payload and revision, and the same renewal behavior as single-job acquisition. The lease starts at that job's acquisition time, not at response delivery.
5. **Independent lifecycle.** Callers can renew, execute, complete, or reschedule each returned lease independently using existing APIs. Acquisition starts no automatic renewal on the caller's behalf. Export/import through the existing remote capability APIs must work as it does for a single acquired lease.

## Count, concurrency, and time bounds

Use the request length as the acquisition count bound. A suggested initial maximum is **100 items**; reject empty or oversized requests before side effects. A separate successful-lease count is unnecessary for this initial API: at most one lease can be acquired per item, and no more jobs are discovered to fill gaps.

The server may acquire jobs concurrently using a bounded implementation-defined limit. A reasonable initial limit is **8 simultaneous acquisitions per request**, subject to backend capacity and service-wide admission limits. Do not require the caller to manage concurrency or require a new public concurrency control in the first version. Request order need not imply acquisition order or priority.

An optional positive `StartWindow` limits how long the server keeps starting acquisition attempts, measured from the start of batch processing. For example, a caller could submit up to 100 jobs with a five-second start window.

**When the count or time limit is reached, stop starting attempts and let in-flight acquisitions finish.** Do not cancel in-flight acquisitions just because the start window elapsed. Include their eventual outcomes in the response. Return `not_attempted` for items that never started. Return immediately when all items are resolved; do not wait out the window or long-poll unavailable jobs.

If omitted, attempt every item subject to ordinary request/service limits. Existing request cancellation and backend timeouts still apply. The start window is not a response deadline: total latency may exceed it while in-flight acquisitions finish. Earlier leases age while the remainder of the batch is processed; the operation does not extend them to compensate.

## Results and failures

A complete response contains exactly one result for every input job, correlated by `JobKey`. Returning results in input order is convenient, but correlation must not depend on completion order.

| Outcome | Meaning |
| --- | --- |
| `acquired` | The attempt acquired the job and returned its usable lease. |
| `not_acquired` | The job could not be leased under the normal single-job rules; this attempt acquired no lease. |
| `not_attempted` | No acquisition was started, for example because the start window ended. |
| `failed` | An error prevented acquisition, and the server can guarantee this attempt acquired no lease. |
| `unknown` | An acquisition may have committed, but the server cannot confirm its outcome or return the lease. |

A batch containing partial success is a normal successful HTTP response. Do not discard usable leases because another item failed. Expose safe error categories rather than credential-bearing diagnostics. Authorization and job-existence disclosure must follow the existing single-job rules.

Validate the request envelope, item syntax, supported options, tenant consistency, and duplicate job identities before acquisition starts. Reject duplicate `JobKey` values even if their routes or worker identities differ. Whole-request validation rejections must guarantee that no item was attempted.

Transport loss or a server failure after processing begins may leave acquired leases whose response never reached the caller. A timeout, missing result, or unusable response is not proof of non-acquisition. Known successful leases remain ordinary leases; an acquired lease that no caller can use recovers through normal expiry. Preserve individually usable results when a client can safely decode and validate them.

Do not automatically replay ambiguous acquisition requests. This feature does not require a durable batch journal, a batch status API, automatic rollback, or idempotent replay. A later explicit call is a new acquisition attempt governed by existing ownership rules; it must not reissue an already-held lease merely because the worker identity matches.

## Scope

The initial feature covers batch acquisition of explicitly selected jobs and its Go/HTTP integration. It requires no application-specific metadata interpretation, workload/resource matching, execution launching, callbacks, activation phase, lease transfer transaction, or knowledge of any consumer's architecture.

Bulk renewal, completion, release, cross-tenant batches, and transactional acquisition can be considered independently. They are not prerequisites for this request. Existing `GetJobLease` and `PollWork` behavior remains available.

## Acceptance criteria

- One request can acquire several explicitly named jobs with distinct worker identities and lease durations; unrelated jobs are never acquired.
- Mixed eligible, unavailable, and route-mismatched jobs produce independent, correctly correlated outcomes. Existing authorization and information-disclosure rules are preserved.
- Concurrent callers cannot both acquire the same active lease, including when batch and single-job APIs race.
- Every successful result contains the authoritative per-lease snapshot and supports existing renewal, completion, rescheduling, and remote export/import paths.
- Count limits and malformed/duplicate/mixed-tenant requests are rejected before side effects.
- Acquisition concurrency is bounded. Expiring the start window prevents new attempts, does not cancel in-flight attempts, and returns their successful leases alongside explicit `not_attempted` results.
- A failure known to precede acquisition is distinguishable from an uncertain backend outcome. Response loss does not trigger automatic replay or bypass lease expiry.
- Go and HTTP conformance tests cover partial success, identity correlation, cancellation, backend errors, lost responses, and credential redaction.

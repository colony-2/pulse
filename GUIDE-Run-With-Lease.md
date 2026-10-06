# Run a job with an existing lease

Use `c2j run with-lease` when a dispatcher has already claimed a JobDB lease and
wants c2j to execute it. The command validates and renews that exact lease before
recipe work, maintains its heartbeat, and exits when the invocation completes,
reschedules, or loses authority. It never claims replacement work.

Ordinary `c2j run`, `run one`, `run any`, and `run loop` retain their existing
claim-and-run behavior. Recipes need no changes.

## Dispatcher handoff

With JobDB v0.0.22 or newer, export a remote lease deliberately:

```go
capability, err := remote.ExportLease(lease)
if err != nil {
    return err
}
encoded, err := capability.Encode()
if err != nil {
    return err
}
// Send encoded through a private pipe, or create an owner-only file.
// Do not print it to logs or place it in command-line arguments.
```

Stop the dispatcher's execution and renewal before starting the receiving worker.
Export does not revoke the dispatcher's copy or transfer exclusive ownership.
The handoff must complete before the lease/capability expires.

The capability contains the tenant, job, lease ID, and credential. The JobDB
connection target is configured separately. Execution state comes from JobDB;
callers should use `ExportLease` and `Encode`, rather than manufacture JSON or
extract private token claims.

## CLI

Given a capability file created with mode `0600` (or `0400`) and owned by the
worker's OS user:

```bash
c2j run with-lease \
  --jobdb https://jobdb.example.com/my-tenant \
  --job-id JOB_ID \
  --lease-file /run/secrets/job-lease.json
```

The file must be a regular file, not a symlink, with no group/other permissions.
For a private pipe or redirected stdin, use `--lease-file -`:

```bash
c2j run with-lease \
  --jobdb https://jobdb.example.com/my-tenant \
  --job-id JOB_ID \
  --lease-file - < /run/secrets/job-lease.json
```

Direct terminal input is rejected. Lease input is limited to 1 MiB. c2j does not
rewrite or delete the supplied file; the dispatcher manages its lifetime. The
file is a handoff credential, not a persistent resume token.

`--job-id` is required and must match the capability. `--jobdb` accepts the same
remote URI/config/environment resolution as other commands; its tenant must also
match. The CLI supports remote capabilities. Library callers can pass already-held
embedded leases directly.

Execution allocation flags such as `--execution-memory`, `--execution-cpu`, and
`--execution-image` still apply. c2j checks allocation under the renewed lease;
incompatible work uses the normal `environment_required` handoff. The original
lease owner and renewal policy are retained, so this command has no `--worker-id`
or `--lease-duration` override. `--await-threshold` still controls when JobDB
reschedules a wait instead of waiting within the current invocation.

## Outcomes

| Result | Exit behavior |
| --- | --- |
| Job completed successfully | Exit 0. |
| Work rescheduled for a future, dependency, or other task route | Emit `job_suspended` JSON and exit 0. The job may still be unfinished. |
| Different execution environment needed | Emit the existing `environment_required` JSON and exit 0. |
| User input required | Default `--input-mode ops` emits `input_required` JSON and exits 3. `--input-mode fail` exits 3 without the form. |
| Invalid, expired, lost, or unrenewable lease; execution/transport failure | Exit nonzero, normally 1; never acquire a replacement lease. |
| Mismatched configured job or tenant | Exit 5 without executing work. |

Story progress is also written to stdout. `--ci` selects progress suitable for
unattended execution. Input is answered through an input client; this command does
not prompt, poll for readiness, or continue after the lease is surrendered. A
later invocation requires a newly dispatched lease. It has no `--on-not-ready`,
`--wait-timeout`, or `--poll-interval` options.

## Go library

Use `executionruntime.Runtime.RunWithLease` to retain c2j's allocation checks and
staged execution requirements while using JobDB's supplied-lease runner:

```go
rt := executionruntime.New(jobDBRuntime, allocation, onHandoff)
worker := compiler.NewRecipeJobWorker(compiler.RecipeJobWorkerOptions{
    Allocation:         allocation,
    StageExecution:     rt.Stage,
    WrapTaskWorker:     rt.WrapTaskWorker,
    OnExecutionHandoff: onHandoff,
    // Supply your normal source resolver, task history, and other options.
})
wrappedTasks := make([]jobworkflow.TaskWorker, len(taskWorkers))
for i, taskWorker := range taskWorkers {
    wrappedTasks[i] = rt.WrapTaskWorker(taskWorker)
}
outcome, err := rt.RunWithLease(ctx, lease, jobworkflow.GetJobForRunRequest{
    JobKey:         lease.Job().JobKey,
    JobWorker:      worker,
    TaskWorkers:    wrappedTasks,
    AwaitThreshold: 30 * time.Second,
}, listener)
```

Here `jobworkflow` is `github.com/colony-2/jobdb/pkg/workflow`; `executionruntime`
and `compiler` are c2j packages. Register the usual schemas/workers and normalize
`allocation` as in ordinary library execution. For remote handoff, obtain `lease`
with `remote.DecodeLeaseCapability(encoded)` and `remoteRuntime.ImportLease(ctx,
capability)`. For embedded use, pass the lease returned by the dispatcher directly.

Leave `WorkerID` empty to preserve the lease owner. A `BeforeRun` hook, if supplied,
runs after allocation admission under the heartbeat's cancellation context. Return
errors or reschedule through the lease passed to the hook; do not start a separate
heartbeat. Treat a suspended outcome as the end of this invocation.

Use `errors.Is(err, jobdb.ErrExecutionLeaseLost)` for lost authority and
`errors.As` with `*jobdb.LeaseRenewalError` for runner renewal failures. Remote
transport failures remain distinguishable through `*remote.LeaseTransportError`.
Unsupported custom leases return `jobdb.ErrLeaseRenewalUnsupported`. All supplied
leases must implement `jobdb.RenewableExecutionLease`.

c2j's wrappers preserve renewed snapshots, current capabilities, and staged
execution demand across heartbeats. Custom wrappers must forward the optional
renewal, expiry, owner, schema, and token methods too; embedding only
`ExecutionLease` is insufficient.

## Deployment and cancellation

Upgrade the JobDB service to support v0.0.22 authoritative lease renewal before
using this c2j version's remote execution paths, including ordinary claim-and-run.
An older remote service fails with unsupported renewal; c2j does not bypass the
validation. Backend adapters must support the corresponding renewal operations.

A renewal failure cancels the execution context and prevents further runner
dispatch. Operations must cooperate with cancellation; arbitrary Go code or
external side effects cannot be forcibly rolled back. JobDB's existing persistence
and ownership guarantees continue to apply. Passing a lease does not introduce
exactly-once side effects.

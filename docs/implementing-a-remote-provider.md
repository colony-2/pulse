# Implementing a remote compute provider

This guide explains how to build a service that Cortex can use to launch containers. It applies both to an adapter around a compute API and to a service that manages its own registered runners.

The service can use any implementation language. Its public interface is the [v1 OpenAPI contract](../api/provider.openapi.yaml); [REMOTE_PROVIDER_PROTOCOL.md](../REMOTE_PROVIDER_PROTOCOL.md) defines the accompanying semantics. Cortex already implements the HTTP client. This repository does not supply a remote provider server or runner-registration service.

## 1. Keep the responsibilities clear

| Cortex owns | Your provider owns | c2j owns |
| --- | --- | --- |
| Finding runnable jobs and selecting one to launch | Matching the requested image, platform, and resources to compute | Claiming the selected job and managing its lease |
| Priority tiers, batch placement, and fallback | Admission, optional queueing, and starting the supplied process | Job execution, replay, and runtime requirement changes |
| Building the process command and environment from the prepared allocation | Execution deadlines, launch idempotency, and inspection records | JobDB state transitions |
| In-memory per-job cooldown | Retaining enough information to recover accepted or uncertain launches | Determining whether the existing allocation remains sufficient |

Treat the process specification and correlation metadata as opaque inputs. Your service does not need to query JobDB, interpret recipes, or construct c2j arguments. If it manages external runners, registration, heartbeats, authentication, and long polling are internal to that service.

A useful first implementation is **immediate admission with no queue**: either commit a compatible execution slot or return `no_capacity`. Add queueing only if your deployment needs it.

## 2. Implement three endpoints

| Endpoint | Required behavior |
| --- | --- |
| `POST /v1/prepare` | Return an expiring plan and concrete allocation for each supported item. Never start or queue execution. |
| `POST /v1/submit` | Accept or decline each prepared launch independently. Preserve the supplied process exactly. |
| `GET /v1/launches/{launch_id}` | Return the launch's allocation, original metadata, execution state, and native references. |

Use HTTPS, `Authorization: Bearer <token>`, and JSON request/response bodies. A configured endpoint identifies one provider instance and admission domain. Scope plans and launch records to the authenticated principal. Replicas behind that endpoint must share the same idempotency and capacity decisions.

For both POST endpoints:

- The envelope is `{"items": [...]}`; the response is `{"results": [...]}`.
- Batches contain 1–100 items. There is no separate single-submit API.
- Validate the envelope, schema, authentication, authorization, and uniqueness of launch IDs **before processing any items**.
- Return HTTP `200` with exactly one result for every input `launch_id`. Match by ID, not array position. Mixed outcomes are normal.
- Reject unknown request fields or options rather than silently discarding constraints. Allow clients to ignore additional response fields.

Do not redirect these endpoints: Cortex deliberately does not follow redirects. An endpoint configured as `https://example.com/compute` receives requests at `/compute/v1/prepare`, `/compute/v1/submit`, and `/compute/v1/launches/{id}`.

## 3. Prepare an allocation

Preparation answers: **if this launch is accepted, what will its container actually receive?**

A request includes:

| Field | Meaning |
| --- | --- |
| `launch_id` | Attempt identity: 1–128 letters, digits, dots, underscores, or hyphens. |
| `image` | OCI image reference, possibly digest-pinned. |
| `platform` | Normalized OS/architecture, optionally with a variant. |
| `cpu_millis` | Usable CPU; 1000 equals one vCPU. |
| `memory_bytes` | Usable application memory. |
| `scratch_bytes` | Usable ephemeral workspace, available simultaneously with the memory allocation. |
| `timeout_seconds` | Maximum execution duration after startup. |
| `start_before` | Optional latest startup time; required if your service queues the launch. |
| `metadata` | Exact string-to-string correlation envelope to retain. |

All resource quantities are positive integer JSON numbers, at most `9007199254740991`. Cortex has already supplied defaults. The current Cortex implementation also limits execution timeouts to one year; your provider can support a smaller range and return `unsupported` for larger requests.

### Allocation rules

1. Match the image and platform. The current client requires `allocation.image` and `allocation.platform` to equal the request values exactly. Keep the requested image reference in `image` even if you bind execution to a resolved digest internally.
2. Round resource capacities **up**, never down. Report the usable quantities you will enforce, not the host's total resources or the requested quantities when you provision something different.
3. Account for resources that share a limit. For example, 2 GiB usable memory plus 1 GiB tmpfs scratch requires room for both; advertising both from a shared 2 GiB allowance is incorrect. Subtract image/system overhead where it consumes the advertised workspace capacity.
4. Use `image_digest` only for an established OCI manifest digest. Use `image_id` for a runtime/config ID, if known; they are different facts. If the request contains `image@sha256:...`, the current client requires the matching verified digest in `allocation.image_digest`. An unsupported explicit constraint must return `unsupported`.
5. Establish an immutable plan binding the principal, launch ID, original request, allocation, deadlines, and metadata. Return its opaque token and expiry. A token can reference a stored plan or carry integrity-protected data; it must not expose credentials.

Preparation may inspect images or make short-lived capacity reservations, but it must not enqueue or launch execution. Reservations must expire automatically. A prepared plan does not guarantee that capacity will still exist at submission time.

### Example: round CPU and memory upward

The timestamps below are illustrative. Use fresh UTC timestamps when trying the example.

**`POST /v1/prepare` request**

```json
{
  "items": [
    {
      "launch_id": "launch-001",
      "image": "registry.example/runner:1.2",
      "platform": "linux/amd64",
      "cpu_millis": 1500,
      "memory_bytes": 1073741824,
      "scratch_bytes": 1073741824,
      "timeout_seconds": 3600,
      "start_before": "2026-09-23T12:00:45Z",
      "metadata": {
        "cortex_metadata_version": "1",
        "cortex_managed_by": "cortex",
        "cortex_jobdb_instance_id": "production",
        "cortex_tenant_id": "acme",
        "cortex_job_id": "job-123",
        "cortex_launch_id": "launch-001"
      }
    }
  ]
}
```

**HTTP `200` response**

```json
{
  "results": [
    {
      "launch_id": "launch-001",
      "status": "prepared",
      "plan": {
        "token": "opaque-plan-001",
        "expires_at": "2026-09-23T12:00:30Z",
        "allocation": {
          "cpu_millis": 2000,
          "memory_bytes": 2147483648,
          "scratch_bytes": 1073741824,
          "platform": "linux/amd64",
          "image": "registry.example/runner:1.2"
        }
      }
    }
  ]
}
```

Preparation can also return `no_capacity`, `unsupported`, `unavailable`, or `rejected`, each with a nonempty `reason`. It does not return `accepted` or `unknown`, because it cannot initiate execution.

## 4. Submit the supplied process

Cortex uses the prepared allocation to construct the executor environment. Submission contains the launch ID, plan token, and process; it does **not** resend the original request or metadata. Your service must recover those from the plan.

**`POST /v1/submit` request**

```json
{
  "items": [
    {
      "launch_id": "launch-001",
      "plan_token": "opaque-plan-001",
      "process": {
        "command": ["c2j"],
        "args": [
          "run", "--jobdb", "https://jobdb.example.com/acme",
          "--job-id", "job-123", "--worker-id", "launch-001",
          "--on-not-ready", "fail", "--ci", "--input-mode", "fail"
        ],
        "env": {
          "C2J_EXECUTION_CPU": "2000m",
          "C2J_EXECUTION_MEMORY": "2097152Ki",
          "C2J_EXECUTION_EPHEMERAL_STORAGE": "1048576Ki",
          "C2J_EXECUTION_PLATFORM": "linux/amd64",
          "C2J_EXECUTION_IMAGE": "registry.example/runner:1.2",
          "CORTEX_METADATA_VERSION": "1",
          "CORTEX_MANAGED_BY": "cortex",
          "CORTEX_JOBDB_INSTANCE_ID": "production",
          "CORTEX_TENANT_ID": "acme",
          "CORTEX_JOB_ID": "job-123",
          "CORTEX_LAUNCH_ID": "launch-001"
        }
      }
    }
  ]
}
```

**HTTP `200` response**

```json
{
  "results": [
    {
      "launch_id": "launch-001",
      "status": "accepted",
      "inspection_uri": "/v1/launches/launch-001",
      "refs": ["runner-pool/executions/exec-789"]
    }
  ]
}
```

An accepted response must contain both `inspection_uri` and `refs`. Return `"refs": []` when no native identifier exists yet; omission and `null` are invalid. Use a relative inspection URI or a same-origin absolute URI. Include the deployment's path prefix when returning a root-relative URI.

### Preserve process semantics

- Execute `command` followed by `args` as an argument vector. Do not join them into a shell string or reinterpret quoting.
- Override the image entrypoint/command with the supplied values. Supplied environment values override the corresponding image environment values.
- Preserve arguments and environment values exactly, including empty strings. Do not recompute `C2J_EXECUTION_*` values or replace the selected job with an arbitrary available job.
- Honor an absolute `working_dir` when supplied; otherwise retain the image's working directory. Return `unsupported` if you cannot honor an option.
- Execute inside the prepared image/platform and enforce the prepared allocation and execution timeout independently of the Cortex process or HTTP connection.

`accepted` means the provider owns the launch: it can be queued, starting, running, or already finished. Saving a plan or creating an inert cloud parent without arranging execution is insufficient. A disconnected caller must not abandon an accepted launch.

## 5. Return capacity and failure outcomes precisely

| Per-item status | Provider guarantee | What Cortex does |
| --- | --- | --- |
| `accepted` | Owns the launch and exposes inspection. | Stops placement for this item. |
| `no_capacity` | No room now; this submission will never start later. | Tries another service. |
| `unsupported` | Cannot honor a required option; nothing will start. | Tries another service. |
| `unavailable` | Temporary failure with confirmed non-acceptance. | Tries another service. |
| `rejected` | Invalid plan/request or configuration problem. | Stops this attempt and reports the reason. |
| `unknown` | Acceptance might have happened. | Stops this attempt without immediate fallback. |

All non-accepted results require a nonempty `reason`. An `unknown` result can include native `refs` if any are known. For a conflicting replay, `rejected` rejects the new request while leaving the original launch intact.

For a five-item submission with capacity for three, return HTTP `200` with five results: three `accepted` and two `no_capacity`. Do not queue the two declined items. Cortex can send those two together to another service. Admission must be atomic against other concurrent batches, including pending starts; idle CPU usage is not evidence that previously committed capacity is free.

### HTTP-level failures

Use `400`, `401`, `403`, `413`, or `422` only for request-level rejection **before processing any items**. The error body is:

```json
{"code": "invalid_batch", "message": "Duplicate launch_id: launch-001"}
```

Once processing starts, use per-item results for known outcomes. A capacity response should be HTTP `200` plus `no_capacity`, rather than HTTP `429` or `503`.

The current Cortex client treats submission timeouts, truncated/invalid JSON, unexpected HTTP statuses (including `429` and `5xx`), and invalid or missing item results as uncertain. It decodes the complete JSON envelope; a truncated response does not salvage an earlier JSON prefix. In an otherwise valid response, individually invalid or missing results cannot authorize fallback. Disable automatic submission retries in gateways or client middleware unless they preserve the same idempotency identity.

Preparation is different: it cannot launch work, so a transport failure or server error can permit fallback. Do not use this distinction as permission to start anything in `/prepare`.

## 6. Make idempotency survive crashes

The idempotency key is:

```text
(provider instance, authenticated principal, launch_id)
```

Cortex preserves a launch ID while trying different providers during one attempt. A later cooldown attempt uses a **new** ID. The provider should not replace this identity with the JobDB job ID; the same job may legitimately need another execution.

A practical submission sequence is:

1. Validate the whole batch before side effects.
2. For each item, serialize access to its idempotency key and look for an existing decision.
3. For a logical replay of the same plan and process, return the recorded result and references. Check this before treating token expiry as a new-submission failure.
4. For conflicting reuse of the ID, return per-item `rejected` without changing the original launch. Compare logical content; JSON object-key and batch-item order must not affect identity. Preserve argument-array order.
5. For a new submission, authenticate the plan, validate its binding/expiry and process, and atomically admit capacity.
6. Record the launch identity, immutable inputs, original metadata, and admission commitment before execution can start. Use native idempotency/deterministic resource identity where available.
7. Arrange execution, retain its decision/references, and return the result. If a native API call may have succeeded, keep the item fenced against another start and report `unknown` until its outcome is resolved.

The record and native API call usually cannot share one transaction. Design explicitly for a crash between them: after restart, reconcile an in-progress record against the native operation before retrying a launch. A lock held only during an HTTP request is insufficient. Use storage or native resources that let your service reconstruct these decisions; a separate database is an implementation choice.

Retention requirements:

- Keep decisions through the plan's expiry.
- Keep accepted launches for their entire lifetime and at least 24 hours after terminal state.
- Keep declined decisions for at least 24 hours as well.
- Keep uncertain submissions fenced while resolving their outcome.
- After a record is removed, its expired plan must still be unable to authorize a new launch.

A replay of a recorded `no_capacity` decision remains that decision even if capacity has since become available. Cortex will use a fresh launch ID for a later attempt.

## 7. Enforce both kinds of deadline

These are three separate clocks:

| Clock | Rule |
| --- | --- |
| Plan `expires_at` | Limits first submission. An expired plan cannot authorize a new launch; a recorded replay can still return its result. |
| Request `start_before` | Latest time execution can start. Expire an accepted launch that misses it without starting the container. |
| Request `timeout_seconds` | Bounds execution duration after startup. Terminate execution when it is exceeded. |

If your service queues accepted work, require `start_before`. When it is absent, either admit directly without queueing or return `unsupported` for queue-only operation. Check the deadline again at actual startup, not just when assigning a runner. Timeouts and deadline enforcement must continue after Cortex disconnects or restarts.

Cortex supplies startup deadlines when `defaults.start_window` is configured. That duration must be positive and no longer than `cooldown`. Neither a token expiry nor an HTTP request timeout substitutes for a startup or execution deadline.

## 8. Preserve reverse lookup and implement inspection

Persist the exact `metadata` map before execution starts, including keys your implementation does not recognize. Cortex currently sends:

| Key | Meaning |
| --- | --- |
| `cortex_metadata_version` | Correlation format version, currently `1`. |
| `cortex_managed_by` | `cortex`. |
| `cortex_jobdb_instance_id` | Configured JobDB instance identity. |
| `cortex_tenant_id` | Tenant identity. |
| `cortex_job_id` | JobDB job identity. |
| `cortex_launch_id` | Cortex launch attempt identity. |

The instance/tenant/job combination identifies the originating job. Treat these values as correlation data, not as credentials or proof of authorization.

Attach the envelope to native resource labels, annotations, tags, or equivalent metadata where possible. If a native system cannot hold it, attach a stable launch-record reference and retain the complete mapping in your service. Someone starting from the native execution must be able to find the originating JobDB entity. Environment variables alone are insufficient for a queued or unassigned launch.

**`GET /v1/launches/launch-001` response**

```json
{
  "launch_id": "launch-001",
  "state": "running",
  "metadata": {
    "cortex_metadata_version": "1",
    "cortex_managed_by": "cortex",
    "cortex_jobdb_instance_id": "production",
    "cortex_tenant_id": "acme",
    "cortex_job_id": "job-123",
    "cortex_launch_id": "launch-001"
  },
  "allocation": {
    "cpu_millis": 2000,
    "memory_bytes": 2147483648,
    "scratch_bytes": 1073741824,
    "platform": "linux/amd64",
    "image": "registry.example/runner:1.2"
  },
  "refs": ["runner-pool/executions/exec-789"],
  "accepted_at": "2026-09-23T12:00:10Z",
  "started_at": "2026-09-23T12:00:12Z"
}
```

Allowed states are `queued`, `starting`, `running`, `succeeded`, `failed`, `timed_out`, `expired`, `cancelled`, and `unknown`. Include `finished_at` and a diagnostic `reason` when appropriate. Provider execution success does not assert that the JobDB job is complete.

Inspection must work immediately after acceptance, including before a runner is assigned; `refs` can then be empty. A `404` means no inspectable record is available, not proof that a prior submission never ran. Cortex's scheduling loop does not poll this endpoint, wait for completion callbacks, or use inspection to bypass cooldown.

## 9. Adapt this to registered external runners

For a service resembling an external CI runner pool:

1. Runners register their supported platforms, image/runtime capabilities, and allocatable resources with your service.
2. `/prepare` checks whether the pool can honor a request and produces an allocation that any eventual selected runner must satisfy.
3. `/submit` commits capacity or a bounded queue entry. If no compatible capacity exists and you prefer Cortex to try another provider, return `no_capacity`.
4. A runner long-polls your internal API for assignments. Assign the immutable plan/process and metadata under an exclusive assignment identity.
5. Before execution starts, verify the assignment remains valid, its startup deadline has not passed, and its allocation can still be honored.
6. Track execution state, enforce the timeout, and retain the correlation record. Release capacity only when you can establish that the execution has stopped or cannot start.

Handle a lost runner connection as an uncertain execution until you can establish its outcome or prevent the old assignment from running. A missed heartbeat alone does not prove the process stopped; immediately reassigning the same launch can create duplicate execution. Your runner protocol needs a way to prevent an old assignment from starting after it has been revoked or replaced.

These runner APIs and their storage remain inside the provider service. Cortex needs no runner-registration endpoint, capacity-count endpoint, notifications, or new persistent state.

## 10. Connect Cortex and verify your service

Example controller configuration:

```yaml
c2j:
  executable: c2j
poll_interval: 5s
cooldown: 60s
call_timeout: 10s
batch_timeout: 1m
defaults:
  image: registry.example/runner:1.2
  platform: linux/amd64
  cpu: "1"
  memory: 1Gi
  scratch: 1Gi
  timeout: 1h
  start_window: 45s
providers:
  runner_pool:
    type: remote
    endpoint: https://runners.example.com
    token_env: CORTEX_RUNNER_TOKEN
targets:
  - instance_id: production
    jobdb: https://jobdb.example.com/acme
    cells: [github.com/acme/app]
    launch_services:
      - name: runner_pool
        priority: 1
```

Supply `CORTEX_RUNNER_TOKEN` in the controller environment, then run:

```sh
cortex -config cortex.yaml -check
cortex -config cortex.yaml -once
```

`-check` validates configuration and c2j, but does **not** make a remote-provider health request. Use `-once` against a seeded test JobDB tenant to exercise actual preparation and submission. For a local test server, `allow_http: true` explicitly permits HTTP; normal deployments use HTTPS.

### Conformance checklist

- Single-item and 100-item batches work; empty/oversized batches, duplicate IDs, and schema failures cause no partial execution.
- Resource rounding is reported accurately; unsupported images, platforms, digests, or process options cannot launch.
- Preparation never starts work, even when its response is lost.
- A partially accepted batch returns one result per ID; declined items never start later.
- Concurrent submissions cannot overcommit the same capacity or start an ID twice.
- Logical replay survives object-key/batch reordering, response loss, and service restart; conflicting replay leaves the original launch unchanged.
- A crash after calling the native launch API is reconciled without blindly starting again.
- Expired plans cannot create new launches; known replays still return their recorded decisions.
- Queued work cannot start after `start_before`; execution stops at its timeout independently of the HTTP caller.
- Inspection works before assignment and preserves all metadata and native references.
- Native execution records can be traced back to the correct JobDB instance, tenant, and job.
- Authentication separates principals; environment secrets and bearer/plan tokens stay out of diagnostic logs.

Validate request/response examples against the OpenAPI schemas as well as testing these behaviors. Schema validation alone cannot prove idempotency, capacity admission, deadline enforcement, or crash recovery.

### Useful code references

- [HTTP client and response handling](../internal/providers/remote/remote.go)
- [Remote client tests](../internal/providers/remote/remote_test.go)
- [Allocation validation and provider types](../pkg/compute/compute.go)
- [Scheduler fallback behavior](../internal/scheduler/scheduler.go)
- [Executable-to-remote integration test](../cmd/cortex/main_test.go)

For a Go server, define wire response types that faithfully implement the OpenAPI schema. In particular, `compute.Submission.Refs` has `omitempty` for in-process use: directly serializing that type with an empty slice omits the required `refs` field on an accepted HTTP result. Likewise, the in-process `PreparedLaunch` type is not the HTTP submission DTO.

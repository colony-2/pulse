# Remote compute provider protocol v1

Cortex uses **one batch submission operation** to send complete container launches. The [OpenAPI contract](api/provider.openapi.yaml) specifies v1 (`1.0.0`) in place; no earlier protocol has been deployed. Built-in adapters use the same submission model in-process. See [Implementing a remote provider](docs/implementing-a-remote-provider.md) for server guidance.

## Operations

| Operation | Purpose |
| --- | --- |
| `POST /v1/submit` | Submit resources, process, deadlines, and metadata together; return an admission result for every item. |
| `GET /v1/launches` | List active instances and their correlation metadata, optionally filtered by launch ID. Read-only; not part of scheduling. |

There is no preparation operation, plan token, or environment binding language. Provider priority and round robin remain Cortex configuration. Runner registration and long polling belong inside the provider service.

Use HTTPS, bearer authentication, and JSON bodies. A configured endpoint identifies one provider instance/admission domain; the authenticated principal scopes launch IDs and listing. Batch size is 1–100, including single-item requests. IDs must be unique within a batch; duplicate IDs invalidate the whole request before any processing. Correlate results by `launch_id`, never array position. HTTP `200` allows mixed outcomes.

## Complete launch requests

Each item contains:

- A unique `launch_id` for this attempt.
- An OCI `image` reference and `platform` (`os/architecture[/variant]`).
- Positive integer `cpu_millis`, `memory_bytes`, and `scratch_bytes`. One CPU is 1000 millicores; the maximum integer is `9007199254740991`.
- `timeout_seconds`, a positive execution limit of at most 31536000 seconds, and optional `start_before` in RFC 3339 format.
- Opaque string-valued `metadata` for reverse lookup.
- `process`: explicit `command`, `args`, literal `env`, and optional absolute `working_dir`. Empty arguments/environment use `[]` and `{}`, not `null`.

Cortex fills omitted resource requirements from deployment defaults, then builds the process environment from those requested values. The provider must guarantee at least the requested usable CPU, memory, and scratch simultaneously. It may round capacities upward or account for overhead internally. That sizing **must not change any supplied environment value**. For a 1500m request provisioned with 2 CPUs, `C2J_EXECUTION_CPU` remains `1500m`. The same complete launch is sent unchanged on fallback.

This reports a conservative guaranteed allocation to c2j: extra capacity from provider rounding is not advertised. A later requirement exceeding the reported allocation can therefore cause a handoff even if it would fit the provider's larger native allocation.

Providers execute the supplied process without interpreting c2j options, expanding environment placeholders, or adding shell interpretation. Supplied values override image/provider defaults; defaults may fill absent keys. Preserve image and platform constraints. If an option cannot be honored, return `unsupported` before accepting work. For a digest-pinned image, Cortex also sets `C2J_EXECUTION_IMAGE_DIGEST` to the requested digest because c2j requires it for compatibility; the provider must enforce that pin before startup. Tag requests omit this field. Listing does not negotiate allocation or feed values back into the submitted environment.

### Example: submit two launches

Send `POST /v1/submit` with `Authorization: Bearer <token>` and `Content-Type: application/json`. This example requests 1500m CPU, 4 GiB application memory, and 1 GiB scratch per launch. The c2j quantity strings describe those same capacities.

<!-- schema: SubmitRequest -->
```json
{
  "items": [
    {
      "launch_id": "launch-001",
      "image": "registry.example/runner:1.2",
      "platform": "linux/amd64",
      "cpu_millis": 1500,
      "memory_bytes": 4294967296,
      "scratch_bytes": 1073741824,
      "timeout_seconds": 3600,
      "start_before": "2026-10-01T12:01:00Z",
      "metadata": {
        "cortex_metadata_version": "1",
        "cortex_managed_by": "cortex",
        "cortex_jobdb_instance_id": "production",
        "cortex_tenant_id": "acme",
        "cortex_job_id": "job-123",
        "cortex_launch_id": "launch-001"
      },
      "process": {
        "command": [
          "c2j"
        ],
        "args": [
          "run",
          "--job-id",
          "job-123",
          "--worker-id",
          "launch-001",
          "--on-not-ready",
          "fail",
          "--ci",
          "--input-mode",
          "fail"
        ],
        "env": {
          "C2J_JOBDB": "https://jobdb.example/acme",
          "C2J_EXECUTION_CPU": "1500m",
          "C2J_EXECUTION_MEMORY": "4194304Ki",
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
    },
    {
      "launch_id": "launch-002",
      "image": "registry.example/runner:1.2",
      "platform": "linux/amd64",
      "cpu_millis": 1500,
      "memory_bytes": 4294967296,
      "scratch_bytes": 1073741824,
      "timeout_seconds": 3600,
      "start_before": "2026-10-01T12:01:00Z",
      "metadata": {
        "cortex_metadata_version": "1",
        "cortex_managed_by": "cortex",
        "cortex_jobdb_instance_id": "production",
        "cortex_tenant_id": "acme",
        "cortex_job_id": "job-124",
        "cortex_launch_id": "launch-002"
      },
      "process": {
        "command": [
          "c2j"
        ],
        "args": [
          "run",
          "--job-id",
          "job-124",
          "--worker-id",
          "launch-002",
          "--on-not-ready",
          "fail",
          "--ci",
          "--input-mode",
          "fail"
        ],
        "env": {
          "C2J_JOBDB": "https://jobdb.example/acme",
          "C2J_EXECUTION_CPU": "1500m",
          "C2J_EXECUTION_MEMORY": "4194304Ki",
          "C2J_EXECUTION_EPHEMERAL_STORAGE": "1048576Ki",
          "C2J_EXECUTION_PLATFORM": "linux/amd64",
          "C2J_EXECUTION_IMAGE": "registry.example/runner:1.2",
          "CORTEX_METADATA_VERSION": "1",
          "CORTEX_MANAGED_BY": "cortex",
          "CORTEX_JOBDB_INSTANCE_ID": "production",
          "CORTEX_TENANT_ID": "acme",
          "CORTEX_JOB_ID": "job-124",
          "CORTEX_LAUNCH_ID": "launch-002"
        }
      }
    }
  ]
}
```

### Example: partial acceptance

HTTP `200 OK`:

<!-- schema: SubmitResponse -->
```json
{
  "results": [
    {
      "launch_id": "launch-001",
      "status": "accepted"
    },
    {
      "launch_id": "launch-002",
      "status": "no_capacity",
      "reason": "No compatible runner slot available."
    }
  ]
}
```

The service owns `launch-001`. Cortex may send only `launch-002` to the next service, using the same complete item and launch ID.

## Submission results and fallback

| Status | Meaning |
| --- | --- |
| `accepted` | Provider admitted the launch to its own bounded queue or handed it to the underlying runtime. This does not guarantee eventual startup or completion. |
| `no_capacity` | No capacity now; nothing launched or queued. Eligible for fallback. |
| `unsupported` | A required option cannot be supplied; nothing launched or queued. Eligible for fallback. |
| `unavailable` | Provider explicitly confirms non-acceptance due to temporary unavailability. Eligible for fallback. |
| `rejected` | Invalid request or configuration problem. Stop this item's attempt and report the reason. |
| `unknown` | Acceptance is uncertain. Stop this item's attempt without immediate fallback. |

Accepted results contain only `launch_id` and `status`. Every non-accepted result also requires a nonempty `reason`. Submission results carry no native references or inspection URL; use `GET /v1/launches?launch_id=...` for active instance details and native references.

All definite declines guarantee that execution cannot later start from that submission. A native failure after possibly initiating execution is `unknown`. Providers using optional native duplicate detection must leave an existing launch unchanged when rejecting a conflicting ID; such a rejection makes no claim about whether that earlier launch ran.

Report `accepted` after admission or handoff has occurred, not merely after validating the request or creating an inert cloud parent. An accepted asynchronous native start operation is sufficient. The provider need not guarantee delivery after a crash, recover lost queue entries, or restart failed launches. Once execution starts, resource limits and `timeout_seconds` must be enforced independently of Cortex.

Batches are not transactions. Preserve valid explicit results in a complete response when another item is missing or malformed. Unaccounted-for IDs, duplicate results, unknown statuses, truncated JSON, timeouts, and transport failures imply uncertainty. Cortex never immediately falls back an uncertain item. A later per-job cooldown attempt can still launch fresh compute with a new ID.

HTTP `400`, `401`, `403`, `413`, and `422` are whole-request rejections **before processing any items**. They stop the attempt. A generic `429`, gateway failure, or `5xx` is not proof of non-acceptance; use HTTP `200` with explicit per-item `no_capacity` or `unavailable` when fallback is safe. Do not blindly retry submission POSTs in middleware.

### Example: whole-request rejection

HTTP `400 Bad Request` for duplicate IDs; no item was processed:

<!-- schema: Error -->
```json
{
  "code": "duplicate_launch_id",
  "message": "Each launch_id must be unique within a batch; no items were processed."
}
```

## Submission attempts and recovery

A `launch_id` identifies one attempt, not the JobDB job's lifetime. Cortex calls each selected launch service at most once for that attempt. After a definite fallback-eligible decline it may submit the same complete item and ID to the next service. It does not retry a timed-out or failed POST, or immediately fall back after an uncertain outcome. A later attempt after the per-job cooldown uses a new launch ID if c2j still reports runnable work.

Clients must not replay a submission to the same provider, including after a lost response. Disable automatic submission retries in HTTP clients, proxies, middleware, and native launch SDKs. A provider makes one attempt to initiate execution for each item; this can require multiple distinct native operations, such as creating a parent and then starting it. It must not retry an ambiguous start, reassign an uncertain dispatch, or run a recovery loop to resubmit failed launches. Reads used for sizing, listing, or awaiting native provisioning are not launch retries.

Providers need no durable submission journal, stored decline decisions, replay cache, or fixed retention period. Native idempotency keys or deterministic resource names may be used as an additional safeguard, but the protocol does not require duplicate detection, comparison of old inputs, or replay of previous results. Duplicate caller submissions are outside this contract.

If an accepted launch is lost or fails to start, c2j/JobDB remains authoritative about outstanding work. Cortex discovers the still-runnable job and can make a fresh attempt after cooldown. If an executor claimed work and then failed, subsequent readiness depends on c2j/JobDB's lease and recovery rules. Providers do not query JobDB or repair jobs, and Cortex does not infer recovery from instance lists.

This is an at-most-once **submission-attempt policy**, not an exactly-once execution guarantee. A runtime may start an earlier attempt late, or Cortex may restart and lose its cooldown. c2j checks readiness, leases, and resource compatibility when a container starts; overlapping attempts can still consume additional compute.

### Handoff, queues, and deadlines

Prefer prompt handoff to the underlying runtime, without a separate provider backlog. Image pulls, scheduling, and startup delays inside that runtime do not require a provider-owned recovery service or a mandatory startup deadline.

A provider that deliberately holds its own queue for runners must require `start_before` and prevent container startup at or after that timestamp. Expire missed queue entries without launching them; no retained terminal record is required. Without a deadline, dispatch directly or return `unsupported` if provider-owned queueing is the only option. Queueing is optional; return `no_capacity` when fallback is preferable.

`start_before`, when supplied, always constrains actual container startup, not just handoff to another system. A provider that cannot enforce it must return `unsupported`. Native cloud handoff can omit it. Cortex sets it through `defaults.start_window`, which must be positive and no longer than `cooldown`. No universal startup deadline is imposed when it is absent. `timeout_seconds` remains a separate limit measured after startup; an HTTP call timeout is neither of these deadlines.

## Active instance listing

`GET /v1/launches` returns only active instances owned by the authenticated principal: `queued`, `starting`, `running`, `paused`, or `stopping`. Exclude succeeded, failed, stopped, cancelled, expired, and timed-out instances. Provider-owned queue entries must be visible before runner assignment. Native inventories may briefly lag handoff. A completed launch disappears from this view; there is no requirement to retain a terminal submission record.

Parameters are optional `page_size` (1–100, default 100), opaque `page_token`, and exact `launch_id`. Each item has a stable provider-local `id`, originating `launch_id`, state, original metadata, and native references. Optional timestamps describe creation/start when known. One launch may have multiple native instances; preserve their distinct IDs.

Return `items: []` for an empty page. If `next_page_token` exists, callers must continue even when the current page has no matching instances. Keep filters unchanged between pages. Listing is a changing view, not an atomic snapshot or historical ledger. Missing instances do not prove non-acceptance, completion, or available capacity. A failed list operation returns an error, never an empty success.

Cortex implements the same list interface for all built-in adapters and exposes it through its [public read-only HTTP API](docs/http-api.md). It does not use listing to bypass cooldown, resolve uncertain submissions, or reserve capacity. Native resource reads, pagination, and credential refresh are allowed; listing must not submit, cancel, restart, or reconcile launches.

### Example: list active instances for one launch

`GET /v1/launches?launch_id=launch-001&page_size=100`, authenticated as the submitting principal, returns HTTP `200 OK`:

<!-- schema: ListResponse -->
```json
{
  "items": [
    {
      "id": "instance-001",
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
      "refs": [
        "runner-pool/runner-17/containers/container-456"
      ],
      "created_at": "2026-10-01T12:00:00Z",
      "started_at": "2026-10-01T12:00:05Z"
    }
  ]
}
```

For an accepted launch awaiting assignment, the same response shape uses `state: "queued"` and `refs: []`. For a terminal or absent launch, return `{"items": []}`. A queued record's `id` must remain stable after assignment; native references can be filled in later.

## Evolution and conformance

The URL carries the major version. Providers reject unrecognized request fields/options rather than silently ignoring constraints. Clients ignore new response fields and treat unknown statuses conservatively. Future breaking changes require a new major URL. V1 has no capacity endpoint, cancellation API, or completion callback requirement.

Conformance covers mixed results, exact process/environment/metadata preservation, resource guarantees, deadlines, duplicate IDs within a batch, single-attempt submission, lost responses and starts without provider retries, truthful declines, and active listing before assignment. The repository supplies the client and contract; it does not include a remote provider server. Documentation examples are checked against the OpenAPI schemas by `scripts/validate_protocol.py`.

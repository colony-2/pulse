# Remote compute provider protocol v1

Cortex uses **one batch submission operation** to send complete container launches. The [OpenAPI contract](api/provider.openapi.yaml) specifies v1 (`1.0.0`) in place; no earlier protocol has been deployed. Built-in adapters use the same submission model in-process. See [Implementing a remote provider](docs/implementing-a-remote-provider.md) for server guidance.

## Operations

| Operation | Purpose |
| --- | --- |
| `POST /v1/submit` | Submit resources, process, deadlines, and metadata together; return an admission result for every item. |
| `GET /v1/launches/{launch_id}` | Inspect a launch and its correlation metadata. Diagnostic only; not part of submission or scheduling. |

There is no preparation operation, plan token, or environment binding language. Provider priority and round robin remain Cortex configuration. Runner registration and long polling belong inside the provider service.

Use HTTPS, bearer authentication, and JSON bodies. A configured endpoint identifies one provider instance/admission domain; the authenticated principal scopes launch IDs and inspection. Batch size is 1–100, including single-item requests. IDs must be unique within a batch; duplicate IDs invalidate the whole request before any processing. Correlate results by `launch_id`, never array position. HTTP `200` allows mixed outcomes.

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

Providers execute the supplied process without interpreting c2j options, expanding environment placeholders, or adding shell interpretation. Supplied values override image/provider defaults; defaults may fill absent keys. Preserve image and platform constraints. If an option cannot be honored, return `unsupported` before accepting work. For a digest-pinned image, Cortex also sets `C2J_EXECUTION_IMAGE_DIGEST` to the requested digest because c2j requires it for compatibility; the provider must enforce that pin before startup. Tag requests omit this field. Other resolved image identities may appear in inspection; they do not feed back into the submitted environment.

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
      "status": "accepted",
      "inspection_uri": "/v1/launches/launch-001",
      "refs": []
    },
    {
      "launch_id": "launch-002",
      "status": "no_capacity",
      "reason": "No compatible runner slot available."
    }
  ]
}
```

The service owns `launch-001`, even though no native reference exists yet. Cortex may send only `launch-002` to the next service, using the same complete item and launch ID.

## Submission results and fallback

| Status | Meaning |
| --- | --- |
| `accepted` | Provider owns the launch; it may be queued, starting, running, or already finished. |
| `no_capacity` | No capacity now; nothing launched or queued. Eligible for fallback. |
| `unsupported` | A required option cannot be supplied; nothing launched or queued. Eligible for fallback. |
| `unavailable` | Provider explicitly confirms non-acceptance due to temporary unavailability. Eligible for fallback. |
| `rejected` | Invalid request or configuration problem. Stop this item's attempt and report the reason. |
| `unknown` | Acceptance is uncertain. Stop this item's attempt without immediate fallback. |

Every non-accepted result requires a nonempty `reason`. Each accepted result requires `inspection_uri` and `refs`; `refs` may be an empty array. Inspection URIs must be relative to the service or same-origin absolute URIs. Native references are diagnostic strings, not scheduling inputs.

All definite declines guarantee that execution cannot later start from that submission. An ID-conflict rejection leaves the original launch unchanged; it does not assert that the original never ran. A native failure after possibly initiating execution is `unknown`.

Acceptance must survive loss of the caller's connection or process. Creating an inert cloud parent is insufficient. Queued launches require `start_before`, and the provider must prevent startup at or after that deadline. A queued launch missing its deadline becomes `expired`. `timeout_seconds` bounds execution after startup and must be enforced independently of Cortex. Return `no_capacity` when fallback is preferable to queueing.

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

## Idempotency and retention

The per-item key is `(provider instance, authenticated principal, launch_id)`. Cortex preserves the ID while falling back within one attempt. A later cooldown attempt uses a new ID.

Serialize concurrent submissions of the same key. During retention, a logical replay of the **entire request and process** returns the recorded decision/references without another execution, independent of batch membership. Ignore JSON object-key order; preserve array order and literal values. Reusing an ID with different resources, image, process, metadata, or deadlines returns a per-item `rejected` conflict without changing the original launch.

Retain accepted decisions throughout the launch's lifetime plus at least 24 hours after termination, and declined decisions for at least 24 hours. Unknown outcomes remain fenced against duplicate execution until resolved. Longer retention is permitted. After retention expires, idempotency is no longer guaranteed: **callers must not replay old submissions**. An elapsed `start_before` prevents a new start but does not erase the decision for a retained replay.

Inspection retains the original metadata even before runner assignment. `404` means no inspectable record is available, possibly because retention expired; it is not proof that a past submission never ran. Cortex uses inspection only for diagnostics and keeps no persistent launch ledger.

### Example: inspect a running launch

`GET /v1/launches/launch-001`, authenticated as the submitting principal, returns HTTP `200 OK`:

<!-- schema: LaunchRecord -->
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
    "memory_bytes": 4294967296,
    "scratch_bytes": 1073741824,
    "platform": "linux/amd64",
    "image": "registry.example/runner:1.2"
  },
  "refs": [
    "runner-pool/runner-17/containers/container-456"
  ],
  "accepted_at": "2026-10-01T12:00:00Z",
  "started_at": "2026-10-01T12:00:05Z"
}
```

Here the provider provisioned 2000m CPU. The submitted `C2J_EXECUTION_CPU` remains `1500m`. For a queued record, `allocation` describes the usable allocation the provider commits to enforce before assignment; `refs` can still be empty. Record manifest digests only when verified; image/config IDs are separate diagnostic facts.

## Evolution and conformance

The URL carries the major version. Providers reject unrecognized request fields/options rather than silently ignoring constraints. Clients ignore new response fields and treat unknown statuses conservatively. Future breaking changes require a new major URL. V1 has no capacity endpoint, cancellation API, or completion callback requirement.

Conformance covers mixed results, exact process/environment/metadata preservation, resource guarantees, deadlines, duplicate IDs, concurrent and conflicting replays, ambiguous responses, retention, and inspection before assignment. The repository supplies the client and contract; it does not include a remote provider server. Documentation examples are checked against the OpenAPI schemas by `scripts/validate_protocol.py`.

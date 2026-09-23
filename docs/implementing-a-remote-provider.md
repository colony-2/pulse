# Implementing a remote compute provider

Build a service that accepts complete container launches through `POST /v1/submit` and lists active instances through `GET /v1/launches`. The service can wrap a cloud API or manage registered runners, in any implementation language.

The [v1 OpenAPI contract](../api/provider.openapi.yaml) defines the wire format. [Remote provider protocol](../REMOTE_PROVIDER_PROTOCOL.md) defines its semantics and includes complete examples of a [batch request](../REMOTE_PROVIDER_PROTOCOL.md#example-submit-two-launches), [partial response](../REMOTE_PROVIDER_PROTOCOL.md#example-partial-acceptance), [request rejection](../REMOTE_PROVIDER_PROTOCOL.md#example-whole-request-rejection), and [list response](../REMOTE_PROVIDER_PROTOCOL.md#example-list-active-instances-for-one-launch). Cortex supplies the HTTP client; this repository does not include a remote provider server.

## Responsibilities

| Cortex owns | Your provider owns | c2j owns |
| --- | --- | --- |
| Finding runnable jobs and selecting one to launch | Matching the requested image, platform, and resources to compute | Claiming the selected job and managing its lease |
| Priority tiers, batch placement, and fallback | Capacity admission, optional queueing, and starting the supplied process | Execution, replay, and runtime requirement changes |
| Building the process and environment from requested/defaulted resources | Resource enforcement, execution deadlines, and active instance listing | JobDB state transitions |
| In-memory per-job cooldown | Prompt native handoff or optional bounded runner queues | Determining whether its reported allocation remains sufficient |

Treat the process and correlation metadata as opaque inputs. Your service does not query JobDB, resolve recipes, or construct c2j arguments. Runner registration, heartbeats, authentication, and long polling are internal to the service.

Start with immediate admission: commit compatible capacity or return `no_capacity`. Add a queue only if the deployment needs it.

## HTTP contract

Use HTTPS, bearer authentication, and JSON bodies. One configured endpoint identifies one provider instance/admission domain. Scope admission and active listings to the authenticated principal. Replicas behind that endpoint must respect the same admission limits, either through the underlying runtime or coordinated capacity accounting. A shared submission history is not required.

For submission:

- Accept `{"items": [...]}` and return `{"results": [...]}`.
- Support batches of 1–100, including a batch of one. Validate the whole envelope, schema, authentication, authorization, and ID uniqueness before processing anything.
- Return HTTP `200` with exactly one result per input ID; mixed outcomes are normal. Correlate by `launch_id`, not position.
- Reject unknown request options. Allow clients to ignore new response fields.
- Do not redirect: Cortex does not follow redirects. An endpoint at `https://example.com/compute` receives `/compute/v1/submit` and `/compute/v1/launches`.

HTTP `400`, `401`, `403`, `413`, and `422` guarantee that no item was processed. Once any item may have been processed, return per-item outcomes or an ambiguous failure instead of one of these whole-request rejections.

## Size and admit complete requests

Each item includes `launch_id`, `image`, `platform`, `cpu_millis`, `memory_bytes`, `scratch_bytes`, `timeout_seconds`, optional `start_before`, `metadata`, and `process`. There are no plan tokens or separate allocation negotiation calls.

1. Match the image and platform. A digest-pinned image must launch that content. Cortex includes its requested digest in `C2J_EXECUTION_IMAGE_DIGEST` for c2j compatibility, so enforcing the pin is part of honoring the submitted environment. Do not silently substitute an image or architecture.
2. Guarantee at least the requested usable CPU, memory, and scratch simultaneously. Round up to native sizes when needed; subtract image/provider overhead where it consumes usable capacity.
3. Account for shared limits. For example, 2 GiB application memory plus 1 GiB tmpfs needs room for both; a shared 2 GiB limit cannot promise both allocations.
4. Preserve supplied command/arguments/environment exactly. Values override image/provider defaults and remain literal; no placeholder expansion or implicit shell. Respect an explicit working directory; otherwise use the image's directory.
5. Admit against committed capacity, including pending and uncertain starts. Do not use momentary utilization as available capacity. Serialize admissions or use an equivalent atomic capacity reservation.

For a 1500m CPU request that your platform rounds to 2 CPUs, keep the supplied `C2J_EXECUTION_CPU=1500m`. Cortex advertises the requested guaranteed allocation, including defaults. Native sizing is internal; the provider does not rewrite c2j's environment to advertise the surplus. Your native tooling can report the larger allocation for diagnostics.

All quantities are positive JSON integers up to `9007199254740991`; timeouts are positive and at most one year. Smaller provider limits can produce `unsupported`. Continue considering later items after one does not fit: a smaller item may still fit.

## Report admission accurately

| Status | Use when |
| --- | --- |
| `accepted` | You admitted the item to your bounded queue or handed it to the underlying runtime. Eventual startup and completion are not guaranteed. |
| `no_capacity` | Nothing was launched or queued, and the pool is full now. |
| `unsupported` | Nothing was launched or queued, and a required option cannot be honored. |
| `unavailable` | You can confirm non-acceptance caused by temporary unavailability. |
| `rejected` | Invalid request/configuration, or an ID conflict detected by optional native safeguards. |
| `unknown` | The operation might have initiated execution, but its outcome is uncertain. |

Accepted results contain only `launch_id` and `status`. Every other status also requires a nonempty diagnostic `reason`. Native references belong in active instance lists, not submission responses.

Definite declines guarantee no delayed execution from that submission. Creating an inert parent resource alone is insufficient for acceptance; an accepted asynchronous start operation can be sufficient. Native parent cleanup is a deployment concern, separate from job recovery.

A lost response after calling a native launch API is usually `unknown`. Do not map generic HTTP `429`, `5xx`, or transport failures to safe fallback. If you know the pool is full, return HTTP `200` with per-item `no_capacity`. Cortex stops uncertain items for this attempt; it can retry the job with a fresh ID after cooldown.

## Make one submission attempt

Cortex submits each launch once to a selected service. It can try a different service after a definite fallback-eligible decline, preserving the launch ID. It does not replay a submission after a timeout or lost response. If work remains runnable after cooldown, Cortex creates a new attempt with a new ID. This limits submissions per attempt, not the number of attempts over a job's lifetime.

A simple provider flow is:

1. Validate the whole batch before side effects, including duplicate IDs within that batch.
2. Check each item's compatibility and capacity. Admit atomically where you own a capacity budget.
3. Attach the immutable process, requirements, and correlation metadata to the native resource or bounded queue entry.
4. Attempt handoff once. Return `accepted` after handoff or queue admission; otherwise report a definite decline only when nothing can subsequently start. Report `unknown` when the outcome is uncertain.
5. Let c2j and Cortex recover outstanding work if this attempt is lost or fails to start. Keep active resources discoverable through native inventory or your runner queue.

Disable automatic POST retries in clients/proxies and launch retries in SDKs. Creating a cloud parent and then starting it are distinct operations within one attempt; polling the parent while it provisions is also allowed. Do not retry an ambiguous start or recreate lost launches after a process restart. Native idempotency tokens or deterministic names are optional safeguards, not a reason to add retries.

There is no requirement to persist submission decisions, record declines, compare replayed inputs, retain terminal records for a fixed period, or reconcile unknown outcomes into a retry. A provider backed by a reliable runtime can submit to it and derive active lists from its resources. Native metadata must remain available while those resources are exposed, but an additional database is unnecessary. Caller replays are outside the contract.

`accepted` reports an admission or handoff that occurred; it is not a promise of eventual execution. A crash that loses an accepted queue entry requires no provider recovery. Work that was never claimed remains visible through c2j; claimed work follows c2j/JobDB lease recovery before another attempt becomes eligible. Resource enforcement and safe accounting for compute that might still be running remain provider responsibilities.

## Handoff and optional bounded queues

Prefer immediate handoff to the underlying runtime. Native image pulls and scheduling delays do not create a requirement for your service to store a durable queue or enforce a universal startup deadline. A late native start still goes through c2j's readiness, lease, and resource checks; overlapping attempts can waste compute.

If your service deliberately queues work for external runners, require `start_before`. Without it, dispatch directly or return `unsupported` if only provider-owned queueing is available. Enforce the deadline at actual startup, including on the runner; admission-time checking alone is insufficient. Expire an entry that misses its deadline without starting it. No durable expiry history is required.

| Clock | Rule |
| --- | --- |
| `start_before`, when supplied | Prevent actual container startup at or after this timestamp. Return `unsupported` if this cannot be enforced, even when handoff itself would be timely. |
| `timeout_seconds` | Terminate execution when its duration after startup reaches the limit, independently of Cortex. |

Cortex supplies startup deadlines when `defaults.start_window` is configured. It must be positive and no longer than `cooldown`; cloud handoff may leave it unset. An HTTP request timeout is not an execution or startup deadline.

## Preserve reverse lookup and list active instances

Retain the exact `metadata` map, including unknown keys, before execution starts. Cortex sends:

| Key | Meaning |
| --- | --- |
| `cortex_metadata_version` | Correlation version, currently `1`. |
| `cortex_managed_by` | `cortex`. |
| `cortex_jobdb_instance_id` | Configured JobDB instance. |
| `cortex_tenant_id` | Tenant. |
| `cortex_job_id` | JobDB job. |
| `cortex_launch_id` | Launch attempt. |

The instance/tenant/job combination identifies the originating entity. Metadata is correlation data, not proof of authorization. Attach it to native labels, annotations, tags, or equivalent fields. If native limits prevent this, attach a stable launch-record reference and retain the full mapping in your service. Environment variables alone cannot identify unassigned launches.

Implement `GET /v1/launches` with optional `page_size` (1–100, default 100), opaque `page_token`, and exact `launch_id` filter. Return `{"items": [...]}` with an optional `next_page_token`. Each item contains a stable provider-local `id`, `launch_id`, state, original metadata, and `refs`; include `created_at` and `started_at` when known. Multiple native instances for one launch have distinct IDs.

Include entries in your own queue immediately after acceptance, before runner assignment. Lists derived from native runtime inventory may briefly lag handoff. Use a stable instance ID and `refs: []` until native references exist. Keep the ID unchanged after assignment. List `queued`, `starting`, `running`, `paused`, and `stopping`; exclude terminal instances such as succeeded, failed, stopped, cancelled, expired, or timed out. Do not substitute an unknown state for a failed lookup: return an error.

Scope every page and cursor to the authenticated principal. Preserve filters between pages. Return `items: []` for an empty page; callers continue if `next_page_token` is present. Cursors must advance, and IDs must be unique within a page. Listing may reflect changes between pages; it need not hold a durable snapshot. An absent launch returns an empty list, including when its retained record is terminal. Absence is not proof that the launch never ran.

Terminal record retention is a deployment choice. Preserve correlation on any native resources you keep, including failed launches, so they remain identifiable while available. The list operation is read-only: it must not submit, cancel, restart, or reconcile work. Cortex exposes lists through its public [HTTP API](http-api.md), without using them to change scheduling or cooldowns. Process environments and credentials do not belong in list responses.

## Adapt this to external runners

1. Runners register platforms, runtime capabilities, and allocatable capacity with your service.
2. `/submit` checks compatibility and commits capacity or a bounded queue entry. Return `no_capacity` when fallback is preferable to waiting.
3. A runner long-polls your internal API. Assign the complete immutable launch under an exclusive assignment identity.
4. Before startup, verify that assignment is still valid, its deadline has not passed, and the requested resources can be guaranteed. Preserve the supplied environment even if the runner allocates more capacity.
5. Track state, enforce timeout, and retain correlation. Release capacity only when execution has stopped or cannot start.

A missed heartbeat does not prove execution stopped or make its capacity safe to reuse. Do not reassign an uncertain dispatch as a retry; let c2j and Cortex determine when a fresh attempt is needed. Runner dispatch leases and c2j job leases have separate responsibilities.

Prefer immediate assignment or a short queue bounded by `start_before`. Accepted queued work can remain runnable in c2j, producing fresh launch IDs after cooldown; these are separate attempts, so optional native deduplication by launch ID does not combine them. Longer queues require provider-owned handling of repeated/stale pending work before enabling them. Cortex needs no registry, long-poll API, or persistent state.

## Connect and verify

```yaml
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

Set `CORTEX_RUNNER_TOKEN`, then run:

```sh
cortex -config cortex.yaml -check
cortex -config cortex.yaml -once
```

`-check` validates configuration and initializes the listing client; it makes no JobDB/provider health request. Use `-once` with seeded test jobs to exercise submission. For development HTTP endpoints, explicitly set `allow_http: true`.

Test single/maximum batches, invalid envelopes without side effects, partial acceptance, resource rounding with unchanged environment, concurrent capacity admission, duplicate IDs within a batch, response loss without launch retries, lost queue entries without provider recovery, deadlines, principal isolation, queued visibility before assignment, pagination (including empty filtered pages), terminal exclusion, and read-only listing. Verify that native resources map back to the originating job. Keep bearer tokens and environment secrets out of logs. Schema validation alone cannot prove these behaviors.

Useful references:

- [HTTP client](../internal/providers/remote/remote.go) and [tests](../internal/providers/remote/remote_test.go).
- [Compute types](../pkg/compute/compute.go) and [scheduler](../internal/scheduler/scheduler.go).
- [CLI integration test](../cmd/cortex/main_test.go) and [schema/example validator](../scripts/validate_protocol.py).

For a Go server, define HTTP DTOs that honor the schema. Complete `compute.Launch` inputs contain request/process data, but the HTTP schema remains authoritative: emit empty arguments/environment as `[]`/`{}`, and reject unknown fields.

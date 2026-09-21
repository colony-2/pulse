# Remote compute provider protocol v1

Status: proposed protocol, specified in [OpenAPI](api/provider.openapi.yaml). Built-in adapters implement the same batch semantics in-process; a remote adapter translates them to HTTP. Provider priority and round robin remain Cortex configuration.

## Operations

| Operation | Purpose |
| --- | --- |
| `POST /v1/prepare` | Size a batch and return allocation facts and opaque, expiring plan tokens for supported items. Does not start or queue execution. |
| `POST /v1/submit` | Submit a batch of prepared plans and process specifications. Return a result for every item. |
| `GET /v1/launches/{launch_id}` | Inspect an accepted launch, including allocation, correlation metadata, and native references. For diagnostics, not the scheduling loop. |

All bodies are JSON. Batch size is 1–100; a single item uses the same endpoints. Results use `launch_id`, never array position, for correlation. IDs must be unique within a batch. Duplicate IDs make the whole request invalid before processing. HTTP `200` permits mixed per-item results; it does not mean every item was accepted.

Use HTTPS and bearer authentication. A configured service endpoint identifies one provider instance/admission domain. The authenticated principal scopes plan tokens, launch IDs, and inspection. Runner registration and long polling are internal to a runner service and are outside this protocol.

## Preparation and allocation

Requests carry image reference, platform, CPU millicores, usable memory/scratch bytes, execution timeout, optional start deadline, and opaque correlation metadata. Cortex fills defaults before sending them. Quantities are positive JSON integers no larger than `9007199254740991`, allowing exact representation by common JSON clients.

Each prepared result contains a token, expiry, and the allocation the provider commits to apply if it accepts submission. Plans bind the original request, including identity, image, capacities, deadlines, and metadata. Tokens may reference provider state or encode a protected plan. They must not expose credentials. Cortex cannot change a plan by modifying its returned allocation object.

The provider can round capacities upward and must report the resulting usable allocation. Image reference, manifest digest, and diagnostic image/config ID are separate fields. Omitted image facts remain unknown. Version 1 requires allocation facts needed by an explicit constraint to be established by preparation; otherwise return `unsupported`. A provider may leave optional digest/config-ID facts absent. Never copy a requested identity as proof of what will run.

Preparation does not guarantee capacity remains available. Any reservation expires automatically. Cortex maps allocation facts into its process environment and submits the token plus an explicit command, argument vector, environment, and optional working directory. Providers execute that specification; they do not interpret c2j options. Image defaults cannot replace supplied process values. Requesting an unsupported process option produces `unsupported`.

An expired token cannot authorize a new launch. Expiry limits when a plan can first be submitted, not how long an already-accepted launch may run. Known idempotent replays return their recorded result even after token expiry.

## Submission results and fallback

| Status | Meaning |
| --- | --- |
| `accepted` | Provider owns the launch. It may be queued, starting, running, or already finished. |
| `no_capacity` | No capacity for this item now; nothing is queued or launched. Eligible for fallback. |
| `unsupported` | A required launch option cannot be supplied; nothing is queued or launched. Eligible for fallback. |
| `unavailable` | Provider explicitly confirms non-acceptance due to temporary unavailability. Eligible for fallback. |
| `rejected` | Invalid request/plan or configuration problem. Stop this item's attempt and report the reason. |
| `unknown` | Acceptance is uncertain. Stop this item's attempt without immediate fallback. |

All definite declines guarantee that execution cannot later start from that submission. A partial native failure after possibly initiating execution is `unknown`, not a decline. Each accepted result supplies an inspection URI; native references are optional if not yet allocated.

Returning `accepted` commits the provider to retaining the launch independently of the caller's connection or process. Creating an inert cloud parent is insufficient. If the provider queues work, `start_before` is required and must be enforced. An accepted item that misses its start deadline becomes `expired` without starting. `timeout_seconds` bounds execution after startup. Capacity exhaustion should return `no_capacity` when the deployment prefers fallback over queueing.

There is no all-or-nothing transaction for a batch. On a truncated response, timeout, transport failure, unexpected HTTP status, or invalid per-item response, treat any unaccounted-for submitted items as uncertain. A completely received, valid per-item result remains usable even if other items fail. Missing preparation results cannot authorize submission; report them as preparation errors.

Documented HTTP `400`, `401`, `403`, `413`, and `422` responses are request-level rejections performed before processing any items. They stop the attempt rather than implying capacity fallback. A generic gateway/server `5xx` is not evidence of non-acceptance. Do not turn it into `no_capacity` or blindly retry POSTs in HTTP middleware.

## Idempotency and retention

The idempotency key is `(provider instance, authenticated principal, launch_id)`, per item rather than per batch. Cortex retains an item's launch ID while trying different providers within one attempt; a later cooldown attempt uses a new ID.

Remote providers must serialize concurrent submissions of the same ID and never start a second execution for an idempotent replay. Retrying the same logical submission returns its recorded decision and references. Reordering JSON object keys or batch items does not change identity. Reusing an ID with a different plan or process returns a per-item `rejected` conflict and leaves the original launch unchanged; this conflict does not mean the original launch never existed.

Keep decisions through the plan's expiry and, for accepted launches, throughout their lifetime plus at least 24 hours after terminal state. Keep declined decisions for at least 24 hours as well. Deployments may retain longer. After decisions are removed, expired tokens still cannot authorize another execution. Unknown submissions must remain fenced against duplicate execution while the provider resolves their outcome.

Inspection returns the original metadata even before runner assignment. `404` means no inspectable record is available; it is not proof that a past submission never ran. Cortex does not need inspection to maintain a persistent launch ledger or resolve ambiguous attempts: the existing cooldown policy still permits a later fresh attempt.

## Evolution and conformance

The URL carries the major protocol version. Use the [OpenAPI 3.1.1 specification](https://spec.openapis.org/oas/v3.1.1.html) for the machine-readable contract. The initial document describes protocol `1.0.0`; it is a design artifact, not an implemented service.

Clients ignore new response fields but treat unknown outcome values conservatively. Providers reject unrecognized request fields/options rather than silently ignoring a requested constraint. Breaking semantic changes require a new major URL. This version has no capabilities endpoint, capacity counter endpoint, cancellation API, or completion callback requirement.

Conformance checks cover partial acceptance, exact allocation/process/metadata preservation, expiry, duplicate IDs, concurrent idempotent replay, conflicting replays, ambiguous responses, and inspection before assignment. Built-in adapters retain equivalent outcomes even if their native APIs differ.

# Pulse HTTP API

Pulse's continuous controller serves public, read-only diagnostics and launch
logs. All endpoints accept `GET` and `HEAD`. Other methods return `405`.
JSON responses carry `Cache-Control: no-store`.

The [controller OpenAPI specification](../api/pulse.openapi.yaml) describes the
routes, parameters, and response schemas.

## Listen address

The default is `:8080`, listening on all interfaces. `PORT` changes the default port; an explicit setting takes precedence:

```yaml
http:
  listen: ":8080"
```

For local access only, use `127.0.0.1:8080`. Publish the container port with `-p 8080:8080` when using Docker. A bind failure stops startup. SIGINT/SIGTERM stops polling and shuts down the listener. `run --once`, `check`, and `version` do not start the HTTP server.

## Routes

| GET path | Content |
| --- | --- |
| `/` | Endpoint index. |
| `/status` | Controller version, uptime, and polling status. |
| `/providers` | Configured providers, advertised bases, capabilities, discovery status, and available health. |
| `/providers/{provider}/capabilities` | Base catalog, discovery status, and available health for one provider. |
| `/providers/health` | Health snapshots for local plugins and reverse providers. |
| `/config` | Parsed configuration including defaults, with secrets redacted. |
| `/instances` | Active instances from all configured providers. |
| `/providers/{provider}/instances` | One page from the named configured provider. |
| `/providers/{provider}/launches/{launch_id}/logs` | Page of launch output. |
| `/scheduler/cooldowns` | Current cooldown entries and in-flight submission attempts. |
| `/scheduler/round-robin` | Priority tiers, batch counters, and next starting services. |

Provider names are configuration keys, such as `runner_pool_a`, or authenticated
identities for [dynamic reverse providers](implementing-a-provider-plugin.md#dynamic-reverse-providers).

### Provider health

`GET /providers/health` publishes local plugin health and current reverse
session health. A configured or remembered reverse provider without a current
session reports `state: unavailable`, with empty implementation and version and
zero protocol and generation. Last-known reverse session details and disconnect reasons are
tracked in [pulse-provider-api issue #1](https://github.com/colony-2/pulse-provider-api/issues/1).

### Provider capabilities

```sh
curl 'http://localhost:8080/providers'
curl 'http://localhost:8080/providers/mac01/capabilities'
```

The inventory lists configured providers and remembered dynamic reverse providers
by name, including disconnected reverse providers. The per-provider route returns
one object with the same fields. Both report the service name and type, discovery
status, advertised base catalog, and health where supported.

<!-- schema: ProviderDetails -->
```json
{
  "name": "mac01",
  "type": "reverse",
  "discovery": "known",
  "bases": [
    {"name": "ubuntu-24.04-arm64", "capabilities": ["docker", "kvm"]},
    {"name": "macos-15-arm64", "capabilities": []}
  ],
  "bases_complete": true,
  "health": {
    "name": "mac01",
    "implementation": "pulse-macvz",
    "version": "example",
    "protocol": 3,
    "state": "ready",
    "restart_count": 0,
    "generation": 1
  }
}
```

Base order matches the provider catalog after Pulse applies base exclusions.
`bases_complete` carries the provider's original completeness flag. Each
base lists the capabilities it can supply. Busy providers can continue to
advertise the same catalog.

Discovery is `known` for a returned catalog, including a complete empty
catalog. `unknown` means the provider supplied no catalog information.
`unavailable`, `timeout`, `invalid`, and `error` describe a failed read.
An empty list can also mean Pulse excluded every advertised base. Use
`discovery` and `bases_complete` alongside the list to interpret it.
HTTP `200` includes the discovery result for each configured provider.

### Launch logs

Set `log_directory` to save live provider output as local JSON Lines files.
A relative directory resolves against the Pulse configuration directory.
Use a separate directory for each controller.
The local Docker provider stores its native archive under the `docker`
subdirectory. Docker retains closed output for seven days by default, with a
1 GiB archive budget per daemon.

```yaml
log_directory: ./launch-logs
```

```sh
curl 'http://127.0.0.1:8080/providers/mac01/launches/LAUNCH_ID/logs?page_size=100'
```

Use the service name and `launch_id` from Pulse's launch result. Anyone who can
reach the HTTP API can read launch logs.

Pulse reads provider-retained logs when available. It falls back to a local
capture for a launch when the provider reports missing or unsupported logs,
or its SDK connection is unavailable. Save the returned `next_page_token`
to continue from the same source. A request with no token starts at the oldest
retained record. `page_size` accepts 1–100 and defaults to 100.

Each record contains `sequence`, `timestamp`, `source`, and base64-encoded
`data` so arbitrary output bytes survive transport. Sources include `launch`,
`stdout`, `stderr`, and `combined`. Decode each record's `data` to reconstruct
the output. Drain pages while `has_more` is true. When caught up, poll with
the latest token until `finalized` is true. `incomplete` indicates known
capture or retention gaps. The optional `estimated_total_size_bytes` estimates
retained payload bytes. These fields describe capture progress.

Local copies persist across Pulse restarts, are capped at 16 MiB of serialized
records per launch, and remain on disk until removed. The first line of each
`<launch_id>.jsonl` file contains archive metadata. A live capture ends when
Pulse exits. Providers that implement retained reads can serve their archives
independently of live capture.

The route returns `400` for an invalid request or cursor, `404` for a missing
log, and `501` when the provider lacks retained logs and local capture is
unconfigured.

### Status

```sh
curl http://localhost:8080/status
```

Example response:

<!-- schema: ControllerStatus -->
```json
{
  "status": "ok",
  "version": "dev",
  "started_at": "2026-10-01T12:00:00Z",
  "uptime_seconds": 12.5,
  "providers": 2,
  "poll": {
    "running": false,
    "last_started": "2026-10-01T12:00:10Z",
    "last_finished": "2026-10-01T12:00:11Z",
    "last_succeeded": true,
    "passes": 3,
    "failed_passes": 0
  }
}
```

`status` is `starting` until a polling pass completes, then `ok` or `degraded` according to the last completed pass. The endpoint returns HTTP `200` in all three cases: it reports controller state without probing providers or JobDB. Counters reflect pass-level errors; individual admission declines are not a provider health check. `running` means a poll is in progress, not that jobs are executing. Timestamps with no observation yet use Go's zero timestamp (`0001-01-01T00:00:00Z`). Error details remain in controller logs. Successful polling does not establish successful placement. Use the correlated [launch-result diagnostic](placement-diagnostics.md) to see requested allocation, original provider errors and retry eligibility; cooldowns are eligibility gates, not scheduled launch times.

### Configuration

`GET /config` reports the selected configuration, whether loaded from a file or `PULSE_CONFIG`, using the YAML field names in a JSON object. The raw `PULSE_CONFIG` value is not returned. It includes selected defaults, targets, provider settings, and HTTP address. Every value under an `env` map is replaced with `[REDACTED]`; variable names remain visible. URLs lose user information and fragments, and any nonempty query is replaced with `redacted`. Token environment-variable names and credential-file paths are configuration and remain visible; credential contents and the controller's process environment are never returned.

### Active instances from one provider

```sh
curl 'http://localhost:8080/providers/runner_pool_a/instances?page_size=100'
curl 'http://localhost:8080/providers/runner_pool_a/instances?launch_id=launch-001'
```

Optional query parameters:

| Parameter | Meaning |
| --- | --- |
| `page_size` | Maximum returned instances, 1–100; default 100. |
| `page_token` | Opaque continuation from the previous response; URL-encode its value. |
| `launch_id` | Exact launch ID filter; keep it unchanged between pages. |

Example response (a remote launch awaiting a runner):

<!-- schema: InstancePage -->
```json
{
  "items": [
    {
      "id": "instance-001",
      "launch_id": "launch-001",
      "state": "queued",
      "job": {
        "jobdb": "https://jobdb.example/acme",
        "instance_id": "production",
        "tenant_id": "acme",
        "job_id": "job-123"
      },
      "refs": [],
      "created_at": "2026-10-01T12:00:00Z"
    }
  ],
  "next_page_token": "opaque-next-page"
}
```

An instance has a provider-local `id`, originating `launch_id`, state, a typed `job` reference, and native `refs`. Creation/start timestamps are optional. Multiple native instances may share a launch ID. Native references can be empty before assignment.

**Include queued, starting, and running instances; exclude terminal instances.** Paused/stopping compute is also included where the provider reports it. Terminal includes succeeded, failed, stopped, cancelled, expired, and timed out. Unknown state is not an active instance. The public API returns the credential-free JobDB endpoint and instance/tenant/job identities in `job`. It never returns leases, process environments, or arbitrary provider metadata. This replaces the previous public `metadata` field; v1 remote/plugin wire formats retain their existing metadata shape.

Always follow `next_page_token`, including after an empty `items` page: native pages may contain only terminal or unrelated resources. Omission ends pagination. Lists reflect current provider observations and can change between requests. They are not a snapshot or history. Native cloud APIs may briefly lag submission/state changes; absence does not prove non-acceptance, completion, or free capacity.

Scope comes from each configured service: remote endpoint/principal, Docker daemon, Cloud Run project/region, ECS cluster, or Azure resource group. Lists include managed resources from previous Pulse processes and other controllers in the same scope. New resources retain their endpoint independently of current targets. Pre-migration resources without an endpoint label need a matching configured instance/tenant target to produce a complete job reference; otherwise listing reports an error. See [provider listing details and read permissions](providers.md#active-instances-and-retention).

### All providers

`GET /instances` reads all pages from each configured provider. It supports the optional `launch_id` filter; `page_size` and `page_token` are only available on the per-provider endpoint. Each item adds a `provider` field. Results are sorted by provider name and native instance ID, with repeated instance IDs deduplicated within each provider.

<!-- schema: CompleteInstanceList -->
```json
{
  "items": [
    {
      "provider": "runner_pool_a",
      "id": "instance-001",
      "launch_id": "launch-001",
      "state": "starting",
      "job": {
        "jobdb": "https://jobdb.example/acme",
        "instance_id": "production",
        "tenant_id": "acme",
        "job_id": "job-123"
      },
      "refs": ["pool/runner-17/container-456"]
    }
  ],
  "errors": [],
  "complete": true
}
```

An unavailable provider, timeout, malformed response, or scan limit returns **HTTP `502` with partial data**, for example:

<!-- schema: PartialInstanceList -->
```json
{
  "items": [],
  "errors": [
    {
      "provider": "runner_pool_a",
      "error": "provider listing failed or scan incomplete"
    }
  ],
  "complete": false
}
```

Inspect `complete` and `errors`; a failing provider is never silently treated as empty. Successfully read pages remain in `items` even if a later page fails. Duplicate configurations sharing a native scope can list the same instance under different provider names.

Each HTTP listing request has a total deadline of `call_timeout`. Pulse permits eight concurrent listing requests, scans up to eight providers at a time within each aggregate request, and caps each provider scan at 1,000 pages or 10,000 unique instances. Large inventories should use the per-provider paginated route. Repeating continuation tokens are errors. Provider-specific scanning limits also apply (Azure bounds native calls while traversing nested jobs/executions).

### Cooldowns

`GET /scheduler/cooldowns` returns active per-job cooldown entries:

<!-- schema: CooldownList -->
```json
{
  "items": [
    {
      "instance": "production",
      "tenant": "acme",
      "job": "job-123",
      "attempted_at": "2026-10-01T12:00:00Z",
      "eligible_at": "2026-10-01T12:01:00Z",
      "in_flight": false
    }
  ]
}
```

Expired entries are hidden unless an attempt is still in flight. `eligible_at` is the cooldown deadline; an in-flight attempt still prevents another submission after that time. Reading does not prune or clear scheduler entries. Failure cooldowns reset on process restart. Accepted/uncertain launches have no cooldown entry after submission; JobDB holds their lease ownership. Definite non-starts also persist backoff when releasing the lease. This endpoint is not a list of active JobDB leases.

### Round-robin state

`GET /scheduler/round-robin` returns each configured service tier, including tiers not yet visited:

<!-- schema: RoundRobinList -->
```json
{
  "items": [
    {
      "scope": "[{\"Name\":\"pool_a\",\"Priority\":1},{\"Name\":\"pool_b\",\"Priority\":1}]",
      "priority": 1,
      "services": ["pool_a", "pool_b"],
      "batches": 3,
      "next_service": "pool_b"
    }
  ]
}
```

`scope` is the serialized launch-service configuration used by scheduling. Targets with identical service sets share rotation. `batches` counts batches entering this tier, including those that decline or fail; `next_service` identifies its next first choice. It is not a prediction of which provider will accept work. Reading does not advance a cursor. State resets on restart.

## Errors and read-only behavior

Malformed listing parameters return `400`, unknown providers/routes return `404`, non-read methods return `405`, and excess concurrent listing requests return `429`. Per-provider listing failures return `502` with a generic error and provider name. Native errors are logged internally rather than exposing credential-bearing diagnostics publicly.

These routes do not submit, cancel, restart, or delete compute; update configuration; reset cooldowns; or change rotation. Listing does not reconcile uncertain starts or modify Docker admission accounting. Credential refresh and native reads are the only provider activity required by diagnostics. Pulse retains no persistent instance inventory and does not use this API to drive job scheduling.

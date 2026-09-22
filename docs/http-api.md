# Cortex HTTP API

Cortex's continuous controller serves public, read-only diagnostics. It does not require authentication. All endpoints accept `GET` and `HEAD`; other methods return `405`. Responses are JSON with `Cache-Control: no-store`. `Access-Control-Allow-Origin: *` allows browser reads without credentials.

## Listen address

The default is `:8080`, listening on all interfaces. `PORT` changes the default port; an explicit setting takes precedence:

```yaml
http:
  listen: ":8080"
```

For local access only, use `127.0.0.1:8080`. Publish the container port with `-p 8080:8080` when using Docker. A bind failure stops startup. SIGINT/SIGTERM stops polling and shuts down the listener. `-once`, `-check`, and `-version` do not start the HTTP server.

## Routes

| GET path | Content |
| --- | --- |
| `/` | Endpoint index. |
| `/status` | Controller version, uptime, and polling status. |
| `/config` | Parsed configuration including defaults, with secrets redacted. |
| `/instances` | Active instances from all configured providers. |
| `/providers/{provider}/instances` | One page from the named configured provider. |
| `/scheduler/cooldowns` | Current cooldown entries and in-flight submission attempts. |
| `/scheduler/round-robin` | Priority tiers, batch counters, and next starting services. |

Provider names are configuration keys, such as `runner_pool_a`, rather than provider types such as `remote`.

### Status

```sh
curl http://localhost:8080/status
```

Example response:

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

`status` is `starting` until a polling pass completes, then `ok` or `degraded` according to the last completed pass. The endpoint returns HTTP `200` in all three cases: it reports controller state without probing providers or JobDB. Counters reflect pass-level errors; individual admission declines are not a provider health check. `running` means a poll is in progress, not that jobs are executing. Timestamps with no observation yet use Go's zero timestamp (`0001-01-01T00:00:00Z`). Error details remain in controller logs.

### Configuration

`GET /config` reports the selected configuration, whether loaded from a file or `CORTEX_CONFIG`, using the YAML field names in a JSON object. The raw `CORTEX_CONFIG` value is not returned. It includes selected defaults, targets, provider settings, and HTTP address. Every value under an `env` map is replaced with `[REDACTED]`; variable names remain visible. URLs lose user information and fragments, and any nonempty query is replaced with `redacted`. Token environment-variable names and credential-file paths are configuration and remain visible; credential contents and the controller's process environment are never returned.

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

```json
{
  "items": [
    {
      "id": "instance-001",
      "launch_id": "launch-001",
      "state": "queued",
      "metadata": {
        "cortex_metadata_version": "1",
        "cortex_managed_by": "cortex",
        "cortex_jobdb_instance_id": "production",
        "cortex_tenant_id": "acme",
        "cortex_job_id": "job-123",
        "cortex_launch_id": "launch-001"
      },
      "refs": [],
      "created_at": "2026-10-01T12:00:00Z"
    }
  ],
  "next_page_token": "opaque-next-page"
}
```

An instance has a provider-local `id`, originating `launch_id`, state, correlation metadata, and native `refs`. Creation/start timestamps are optional. Multiple native instances may share a launch ID. Native references can be empty before assignment.

**Include queued, starting, and running instances; exclude terminal instances.** Paused/stopping compute is also included where the provider reports it. Terminal includes succeeded, failed, stopped, cancelled, expired, and timed out. Unknown state is not an active instance. The public API returns only the six correlation metadata fields shown above, never process environments or arbitrary provider metadata.

Always follow `next_page_token`, including after an empty `items` page: native pages may contain only terminal or unrelated resources. Omission ends pagination. Lists reflect current provider observations and can change between requests. They are not a snapshot or history. Native cloud APIs may briefly lag submission/state changes; absence does not prove non-acceptance, completion, or free capacity.

Scope comes from each configured service: remote endpoint/principal, Docker daemon, Cloud Run project/region, ECS cluster, or Azure resource group. Lists include managed resources from previous Cortex processes and other controllers in the same scope. They are not limited to current target cells. See [provider listing details and read permissions](providers.md#active-instances-and-retention).

### All providers

`GET /instances` reads all pages from each configured provider. It supports the optional `launch_id` filter; `page_size` and `page_token` are only available on the per-provider endpoint. Each item adds a `provider` field. Results are sorted by provider name and native instance ID, with repeated instance IDs deduplicated within each provider.

```json
{
  "items": [
    {
      "provider": "runner_pool_a",
      "id": "instance-001",
      "launch_id": "launch-001",
      "state": "starting",
      "metadata": {
        "cortex_jobdb_instance_id": "production",
        "cortex_tenant_id": "acme",
        "cortex_job_id": "job-123",
        "cortex_launch_id": "launch-001"
      },
      "refs": ["pool/runner-17/container-456"]
    }
  ],
  "errors": [],
  "complete": true
}
```

An unavailable provider, timeout, malformed response, or scan limit returns **HTTP `502` with partial data**, for example:

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

Each HTTP listing request has a total deadline of `call_timeout`. Cortex permits eight concurrent listing requests, scans up to eight providers per aggregate request, and caps each provider scan at 1,000 pages or 10,000 unique instances. Large inventories should use the per-provider paginated route. Repeating continuation tokens are errors. Provider-specific scanning limits also apply (Azure bounds native calls while traversing nested jobs/executions).

### Cooldowns

`GET /scheduler/cooldowns` returns active per-job cooldown entries:

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

Expired entries are hidden unless an attempt is still in flight. `eligible_at` is the cooldown deadline; an in-flight attempt still prevents another submission after that time. Reading does not prune or clear scheduler entries. Cooldowns reset on process restart.

### Round-robin state

`GET /scheduler/round-robin` returns each configured service tier, including tiers not yet visited:

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

These routes do not submit, cancel, restart, or delete compute; update configuration; reset cooldowns; or change rotation. Listing does not reconcile uncertain starts or modify Docker admission accounting. Credential refresh and native reads are the only provider activity required by diagnostics. Cortex retains no persistent instance inventory and does not use this API to drive job scheduling.

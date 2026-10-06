# Pulse

**Run c2j jobs on cloud compute or your own runners.**

Pulse watches all repositories in a JobDB tenant, finds jobs that need an executor, acquires a JobDB lease for each selected job, and passes it to the container. It uses c2j's public Go library for discovery and tags each launch with its JobDB identity. The controller needs no separate c2j executable; **executor images must contain c2j and the tools their recipes need**.

[Install](#install) · [Quick start](#quick-start) · [Run in a container](#run-in-a-container) · [Configuration](#configuration) · [Providers](#providers) · [HTTP API](#http-api)

## Install

### npm

```sh
npm install --global @colony2/pulse
pulse version
```

Requires Node.js 22+, `tar`, enabled install scripts, and HTTPS access to GitHub Releases. The installer downloads the matching native executable and Linux container helper, verifies the archive SHA-256 checksum, and installs both together.

### Native executable

Download your platform's archive from [GitHub Releases](https://github.com/colony-2/pulse/releases), verify it against `checksums.txt`, and put `pulse` on your `PATH`. Native executables need no Node.js. All archives also contain a Linux `pulse-exec` helper for containers; keep it beside `pulse` when using local Docker.

Linux and macOS are supported on AMD64 and ARM64. Container images support Linux AMD64 and ARM64. The local Docker provider supports Linux servers and Docker Desktop on macOS, using Pulse's automatic Docker socket/context discovery.

## Quick start

Start Docker Engine on Linux or Docker Desktop on macOS, then run:

```sh
pulse
```

No Pulse configuration file is required. Pulse uses the same tenant settings as c2j: `C2J_JOBDB`, then `jobdb` in the nearest `.c2j/config.yaml`. You can also select a tenant explicitly:

```sh
pulse run --jobdb https://jobdb.example.com/acme
pulse check
pulse run --once
pulse version
```

Pulse discovers eligible c2j jobs across every repository in the tenant. Docker is the default provider, and jobs without an image use `ghcr.io/colony-2/shai-mega:latest`. The default job requests 1 CPU, 1 GiB memory, and 1 GiB scratch; the Docker pool runs one job at a time. Docker's native Linux platform is detected automatically.

`check` validates settings and initializes providers; it does not verify remote connectivity or launch permissions. `run --once` performs one discovery/submission pass. Both `pulse` and `pulse run` keep polling until SIGINT or SIGTERM and write JSON logs to stderr. Launched containers continue under their own lifetime controls.

For custom providers or multiple tenants, use an optional configuration: [remote runners](examples/remote.yaml), [local Docker overrides](examples/docker.yaml), or [cloud providers](examples/clouds.yaml). Executor images must include c2j with `run with-lease` support; cloud images also need `pulse-exec`. Use JobDB v0.0.22 or later with supplied-lease renewal support.

## Run in a container

Use **`ghcr.io/colony-2/pulse`**. For simple deployments, pass the complete YAML in **`PULSE_CONFIG`**. Start with [examples/container.yaml](examples/container.yaml), fill in the settings, then run:

```sh
export PULSE_CONFIG="$(cat pulse.yaml)"
docker run --rm --init \
  --read-only --tmpfs /tmp -p 8080:8080 \
  -e PULSE_CONFIG -e PULSE_PROVIDER_TOKEN \
  ghcr.io/colony-2/pulse:latest
```

The file is read on the host. In a deployment console, set `PULSE_CONFIG` directly to the YAML contents. Keep credentials in separate secret/environment settings or use the hosting platform's identity. For repeatable deployments, select a release tag such as `:vX.Y.Z` or an image digest.

The image is distroless, runs as UID/GID `65532:65532`, and contains Pulse, its execution supervisor, and CA certificates. It includes native cloud authentication and needs no shell, Node.js, Git, cloud CLI, or separate c2j executable. Configure `defaults.image` with your workload's executor image.

For mounted YAML, pass the file path explicitly:

```sh
docker run --rm --init --read-only --tmpfs /tmp -p 8080:8080 \
  --mount "type=bind,source=$PWD/pulse.yaml,target=/etc/pulse/pulse.yaml,readonly" \
  -e PULSE_PROVIDER_TOKEN \
  ghcr.io/colony-2/pulse:latest --config /etc/pulse/pulse.yaml
```

The mounted file must be readable by UID 65532. See [configuration and Cloud Run deployment](docs/configuration.md), [cloud authentication](docs/cloud-authentication.md), and [Docker socket/helper setup](docs/providers.md).

## Configuration

Pulse optionally loads one YAML document, in order:

1. Explicit `--config PATH`.
2. `PULSE_CONFIG` containing the YAML itself.
3. `./pulse.yaml`, if present.
4. Built-in defaults.

Sources are not merged. Empty or invalid inline configuration fails startup unless an explicit file was selected. Values are literal, without environment-placeholder expansion. Restart or redeploy to apply changes.

Tenant selection uses `--jobdb`, an explicit Pulse target's `jobdb`, `C2J_JOBDB`, then c2j project settings. Each target covers the whole tenant. Missing `instance_id` values are derived from the JobDB deployment URL. Remove obsolete `cells` settings when upgrading. Pulse requires an HTTP(S) JobDB tenant; c2j's embedded database cannot serve container workers.

Launch services have positive numeric priorities: **1 is preferred over 2**. Equal-priority services rotate first choice for each batch. A provider can accept part of a batch and decline the rest for fallback.

```yaml
targets:
  - instance_id: production
    jobdb: https://jobdb.example.com/acme
    jobdb_token_env: JOBDB_TOKEN
    launch_services:
      - {name: runner_pool_a, priority: 1}
      - {name: runner_pool_b, priority: 1}
      - {name: cloud_overflow, priority: 2}
```

Define these service names under `providers` and supply `JOBDB_TOKEN` for authenticated JobDB discovery and lease acquisition. Pulse never passes this broad credential to the executor. Omit `jobdb_token_env` for an unauthenticated deployment. Provider authentication is configured separately.

When `providers` is omitted or empty, Pulse defaults to a local provider named `docker`; targets without `launch_services` use it at priority 1. The default pool runs one job at a time, with a budget sized for the default job plus 256 MiB memory overhead. Install the Linux `pulse-exec` helper beside `pulse` or on `PATH`, and start Docker Engine or Docker Desktop. See [Docker defaults and overrides](docs/providers.md#local-docker-capacity).

Defaults are a 5-second poll interval, a 5-minute lease duration, 60-second provisioning-failure backoff, and batches of at most 100 jobs. Claims run with up to 8 concurrent calls and a 5-second claim window; configure these with `claim_concurrency`, `claim_timeout`, and `batch_size`. Pulse acquires the lease before submission and does not renew it; c2j renews the supplied lease on startup. Accepted or uncertain starts rely on lease expiry for recovery. Definite non-starts release the lease with backoff. In-memory cooldowns suppress failures only; JobDB ownership survives controller restarts. See [configuration details](docs/configuration.md) and the complete [examples](examples).

## Providers

| Provider | Type | Notes |
| --- | --- | --- |
| Local Docker | `docker` | CPU, memory, and container-count budgets; no disk quotas. |
| Remote runner service | `remote` | Batch submission with partial acceptance and capacity fallback. |
| Google Cloud Run Jobs | `cloudrun` | Native job execution and supported resource sizes. |
| AWS ECS Fargate | `ecs` | Standalone tasks with the execution supervisor. |
| Azure Container Apps Jobs | `azurejobs` | Manual jobs with native execution timeouts. |

In cloud environments, attach a service account, task/instance role, or managed/workload identity to the controller. Elsewhere, supply supported environment credentials or mounted credential files. See [cloud authentication](docs/cloud-authentication.md) and [provider operations](docs/providers.md) for permissions, image requirements, capacity, and resource cleanup.

## HTTP API

Continuous mode serves a public, unauthenticated, read-only API on `:8080`. `PORT` changes the default port; `http.listen` can override the address.

| GET endpoint | Returns |
| --- | --- |
| `/status` | Version, uptime, and polling status. |
| `/config` | Configuration with environment values and URL credentials redacted. |
| `/instances` | Active instances across all configured providers. |
| `/providers/{provider}/instances` | One page from a configured provider. |
| `/scheduler/cooldowns` | Current cooldowns and in-flight attempts. |
| `/scheduler/round-robin` | Priority tiers and next starting services. |

```sh
curl http://localhost:8080/status
curl 'http://localhost:8080/providers/runners/instances?page_size=100'
```

Lists include queued, starting, and running instances and exclude terminal instances; paused/stopping compute is included where supported. `run --once`, `check`, and `version` do not start the server. See [HTTP API documentation](docs/http-api.md) for pagination and partial failures.

## More documentation

- [Configuration and container deployment](docs/configuration.md)
- [Provider operations](docs/providers.md)
- [Implementing a remote provider](docs/implementing-a-remote-provider.md)
- [Contributing and development](CONTRIBUTING.md)

## License

[Apache-2.0](LICENSE).

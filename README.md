# Pulse

**Run c2j jobs on cloud compute or your own runners.**

Pulse watches configured repository cells, finds jobs that need an executor, and starts a container for each selected job. It uses c2j's public Go library for discovery and tags each launch with its JobDB identity. The controller needs no separate c2j executable; **executor images must contain c2j and the tools their recipes need**.

[Install](#install) · [Quick start](#quick-start) · [Run in a container](#run-in-a-container) · [Configuration](#configuration) · [Providers](#providers) · [HTTP API](#http-api)

## Install

### npm

```sh
npm install --global @colony2/pulse
pulse -version
```

Requires Node.js 22+, `tar`, enabled install scripts, and HTTPS access to GitHub Releases. The installer downloads the matching native executable and verifies its SHA-256 checksum.

### Native executable

Download your platform's archive from [GitHub Releases](https://github.com/colony-2/pulse/releases), verify it against `checksums.txt`, and put `pulse` on your `PATH`. Native executables need no Node.js. Linux archives also contain the `pulse-exec` helper used by Docker and ECS.

Linux and macOS are supported on AMD64 and ARM64. Container images support Linux AMD64 and ARM64. The local Docker provider requires a Linux controller.

## Quick start

1. Choose an executor image containing c2j and your recipe dependencies.
2. Copy a configuration example: [remote runners](examples/remote.yaml), [local Docker](examples/docker.yaml), or [cloud providers](examples/clouds.yaml).
3. Save it as `pulse.yaml` and fill in the JobDB tenant URL, repository cells, default executor image, and provider settings.

For the remote-runner example:

```sh
export PULSE_PROVIDER_TOKEN='your-provider-token'
pulse -config pulse.yaml -check
pulse -config pulse.yaml -once
pulse -config pulse.yaml
```

`-check` validates configuration and initializes clients/providers; it does not verify remote connectivity or launch permissions. `-once` performs one discovery/submission pass. Normal mode keeps polling until SIGINT or SIGTERM and writes JSON logs to stderr. Launched containers continue under their own lifetime controls.

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
  ghcr.io/colony-2/pulse:latest -config /etc/pulse/pulse.yaml
```

The mounted file must be readable by UID 65532. See [configuration and Cloud Run deployment](docs/configuration.md), [cloud authentication](docs/cloud-authentication.md), and [Docker socket/helper setup](docs/providers.md).

## Configuration

Pulse selects one complete YAML document, in order:

1. Explicit `-config PATH`.
2. `PULSE_CONFIG` containing the YAML itself.
3. `./pulse.yaml`.

Sources are not merged. Empty or invalid inline configuration fails startup unless an explicit file was selected. Values are literal, without environment-placeholder expansion. Restart or redeploy to apply changes.

Targets select a JobDB instance/tenant and explicit repository identities, such as `github.com/acme/app` or `file:///absolute/repository/path`. These must match the repository metadata used when submitting jobs. Pulse does not discover local checkouts or resolve aliases.

Launch services have positive numeric priorities: **1 is preferred over 2**. Equal-priority services rotate first choice for each batch. A provider can accept part of a batch and decline the rest for fallback.

```yaml
targets:
  - instance_id: production
    jobdb: https://jobdb.example.com/acme
    jobdb_token_env: JOBDB_TOKEN
    cells: [github.com/acme/api, github.com/acme/worker]
    launch_services:
      - {name: runner_pool_a, priority: 1}
      - {name: runner_pool_b, priority: 1}
      - {name: cloud_overflow, priority: 2}
```

Define these service names under `providers` and supply `JOBDB_TOKEN` for authenticated JobDB access. Omit `jobdb_token_env` for an unauthenticated deployment. Provider authentication is configured separately.

Defaults are a 5-second poll interval, a 60-second per-job cooldown, and batches of at most 100 jobs. Pulse stores cooldowns and rotation in memory. A restart or uncertain launch can result in duplicate compute; c2j/JobDB leases govern job ownership. See [configuration details](docs/configuration.md) and the complete [examples](examples).

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

Lists include queued, starting, and running instances and exclude terminal instances; paused/stopping compute is included where supported. `-once`, `-check`, and `-version` do not start the server. See [HTTP API documentation](docs/http-api.md) for pagination and partial failures.

## More documentation

- [Configuration and container deployment](docs/configuration.md)
- [Provider operations](docs/providers.md)
- [Implementing a remote provider](docs/implementing-a-remote-provider.md)
- [Contributing and development](CONTRIBUTING.md)

## License

[Apache-2.0](LICENSE).

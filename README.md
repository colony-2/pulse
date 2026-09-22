# Cortex

**Container compute for c2j jobs, across cloud services and your own runners.**

Cortex watches explicitly configured repository cells for jobs that need an executor. It selects a launch service, supplies the job's execution requirements, and starts a container running `c2j` for that job. Every launch carries metadata linking it back to its JobDB instance, tenant, and job.

[Quick start](#quick-start) · [Container image](#container-image) · [Providers](#providers) · [Configuration](#configuration) · [Releases](docs/releases.md)

## How it works

```mermaid
flowchart LR
    J[JobDB] -->|c2j list --json| C[Cortex]
    C -->|Prepare and submit batches| P[Launch services]
    P --> D[Local Docker]
    P --> R[Remote runner service]
    P --> G[Cloud Run / ECS / Azure Jobs]
    D & R & G --> E[Container: c2j run --job-id]
    E -->|Claim and execute job| J
```

- **Small controller:** no database, notifications, or runner registry. Polling, per-job cooldowns, and round-robin cursors stay in memory.
- **Portable requirements:** image, architecture, CPU, memory, and scratch capacity come from c2j, with configured defaults when absent. Executors receive the allocation the provider actually supplies.
- **Batch placement:** services accept or decline individual jobs in a batch. Cortex tries equal-priority peers before moving to a lower-priority tier.
- **Clear ownership:** c2j manages job leases, replay, and changes in execution requirements. Cortex supplies compute through the c2j executable interface.

## Install

### npm

```sh
npm install --global @colony2/cortex @colony2/c2j
cortex -version
c2j version
```

The release workflow publishes `@colony2/cortex`. Its installer downloads the matching native executable from GitHub Releases and verifies its SHA-256 checksum. Node.js 22 or newer, `tar`, and HTTPS access to GitHub release assets are required. Install scripts must be enabled. `c2j` is a separate executable dependency; the container image bundles it.

| Platform | AMD64 / x86-64 | ARM64 / Apple Silicon |
| --- | --- | --- |
| Linux | ✓ | ✓ |
| macOS | ✓ | ✓ |
| Container image (Linux) | ✓ | ✓ |

This matches c2j's release matrix. Windows packages are not currently supplied. The local Docker provider requires a Linux controller; the remote and cloud adapters are also available on macOS.

### Native executables

Download the archive for your platform from [GitHub Releases](https://github.com/colony-2/cortex/releases), verify it against `checksums.txt`, and put `cortex` on your `PATH`. Linux archives also include the static `cortex-exec` helper used by the Docker and ECS providers. A standalone Cortex executable does not require Node.js.

## Quick start

1. Install Cortex and a compatible c2j release with execution support.
2. Copy [examples/remote.yaml](examples/remote.yaml) to `cortex.yaml`. Set your JobDB tenant URL, repository cells, runner-service endpoint, and default executor image.
3. Supply the provider token and start Cortex:

```sh
export CORTEX_PROVIDER_TOKEN='your-provider-token'
cortex -config cortex.yaml -check
cortex -config cortex.yaml -once
cortex -config cortex.yaml
```

`-check` validates configuration, checks c2j's execution flags, and initializes providers. `-once` performs one polling pass. The normal mode keeps polling until SIGINT or SIGTERM. Logs are JSON on stderr; containers already launched continue under their own lifetime controls.

For a local machine, start with [examples/docker.yaml](examples/docker.yaml). For cloud services, use [examples/clouds.yaml](examples/clouds.yaml).

## Container image

Releases publish **`ghcr.io/colony-2/cortex`** for `linux/amd64` and `linux/arm64`. Docker chooses the matching architecture automatically.

The image uses **distroless static Debian**, runs as UID/GID `65532:65532`, and contains:

- `/usr/local/bin/cortex`
- `/usr/local/bin/c2j` — the latest stable c2j release resolved when the Cortex release is built
- `/usr/local/bin/cortex-exec`
- CA certificates and bundled version/license records under `/usr/share/cortex`

Copy [examples/container.yaml](examples/container.yaml) to `cortex.yaml`, fill in the deployment values, and run:

```sh
docker run --rm --init \
  --read-only --tmpfs /tmp \
  --mount "type=bind,source=$PWD/cortex.yaml,target=/etc/cortex/cortex.yaml,readonly" \
  -e CORTEX_PROVIDER_TOKEN \
  ghcr.io/colony-2/cortex:latest
```

The mounted configuration must be readable by UID 65532. For repeatable deployments, select a release tag such as `:vX.Y.Z` or a manifest digest. `latest` advances after a successful release; an existing image keeps its bundled c2j version. Both architectures use the same c2j release, recorded in the image label `com.colony2.c2j.version` and the GitHub release's `versions.txt`.

Inspect the executables without a shell:

```sh
docker run --rm ghcr.io/colony-2/cortex:latest -version
docker run --rm --entrypoint /usr/local/bin/c2j ghcr.io/colony-2/cortex:latest version
```

**Provider dependencies inside the image:** remote services work directly. Cloud Run and Azure support access tokens through `token_env`; token renewal must be handled by the deployment. The image does not contain `gcloud`, `az`, or `aws`. The ECS adapter requires AWS CLI v2, so use a deployment image supplying that CLI. Local Docker needs socket permissions, a shared host lock directory, and a supervisor path visible at the same absolute location to the controller and daemon. See [provider operations](docs/providers.md).

This is a controller image. Configure `defaults.image` for your workload's executor image, including c2j and any shell, Git, or language tools required by its recipes. The controller image itself contains no shell or Git; use explicit repository selectors in its configuration.

### Build an image locally

Resolve c2j before building so both architectures use one version and the build cache can distinguish upgrades:

```sh
C2J_VERSION=$(python3 scripts/fetch_c2j.py)
docker buildx build --load \
  --build-arg "C2J_VERSION=$C2J_VERSION" \
  --build-arg VERSION=dev \
  -t cortex:local .
```

For a multi-architecture registry build, replace `--load` with `--platform linux/amd64,linux/arm64 --push` and use your registry tag. GitHub Releases also include per-architecture container archives that can be imported with `docker load --input <archive.tar.gz>`.

## Providers

| Provider | Configuration type | Capacity and placement |
| --- | --- | --- |
| Local Docker | `docker` | Explicit CPU, memory, and container-count budget; reconstructs commitments from container metadata. No disk quotas. |
| Remote service | `remote` | Authenticated [OpenAPI protocol](api/provider.openapi.yaml), batch submission, partial acceptance, and capacity fallback. |
| Google Cloud Run Jobs | `cloudrun` | Rounds up to supported job resource sizes; native execution timeout. |
| AWS ECS Fargate | `ecs` | Standalone tasks with supported task sizes and the execution supervisor. |
| Azure Container Apps Jobs | `azurejobs` | Manual jobs with supported CPU/memory pairs and native execution timeout. |

A future service that registers external runners and dispatches work by long polling can implement the remote protocol. Cortex needs only its prepare/submit API.

See [provider configuration and limitations](docs/providers.md) for authentication, image storage bounds, Docker admission, retention, and cloud allocation details.

## Configuration

Each target selects a JobDB instance/tenant and a list of repository cells. Launch services have positive numeric priorities: **1 is preferred over 2**. Services at the same priority rotate first choice per batch.

```yaml
targets:
  - instance_id: production
    jobdb: https://jobdb.example.com/acme
    cells: [github.com/acme/api, github.com/acme/worker]
    launch_services:
      - {name: runner_pool_a, priority: 1}
      - {name: runner_pool_b, priority: 1}
      - {name: cloud_overflow, priority: 2}
```

Define those service names under `providers` in the same file. Full examples include [remote](examples/remote.yaml), [Docker](examples/docker.yaml), [cloud](examples/clouds.yaml), and [container](examples/container.yaml) configurations.

Defaults include a 5-second poll interval, a 60-second per-job cooldown, and batches of at most 100 jobs. A confirmed `no_capacity`, `unsupported`, or `unavailable` response permits fallback. An uncertain submission retains its cooldown without immediate fallback, since compute may already have started.

Only recipe job routes are selected by default. Cell selectors must match the repository metadata used at submission; a local path and its Git remote selector need not be interchangeable. Use explicit repository selectors in container deployments. `c2j.expected_version` optionally pins the **complete** output of `c2j version`, for example `c2j version 0.0.52` for that official release binary.

A controller restart loses its cooldown and may cause duplicate compute. JobDB leases protect job ownership; job side effects still need their normal idempotency. Provider resources are retained for inspection; configure terminal-resource cleanup for your deployment.

## Development

Go 1.24 or newer is required. Packaging tests also use Node.js 22+, Python 3, and standard Unix archive tools.

```sh
make build          # bin/cortex and bin/cortex-exec
make test           # Go tests with the race detector
make vet
make test-packaging # npm installer and c2j download verification
```

The suite covers scheduler concurrency/fallback, CLI-to-remote integration, Docker accounting/recovery, cloud request mappings, and package installation. CI runs on Linux AMD64, Linux ARM64, and macOS, cross-compiles all four release executables, and smoke-tests both container architectures.

The c2j JSON contract has also been checked against c2j v0.0.52 and a temporary JobDB server. To repeat that read-only integration check against your own seeded test tenant:

```sh
CORTEX_TEST_C2J=/path/to/c2j \
CORTEX_TEST_JOBDB=http://127.0.0.1:8080/test-tenant \
CORTEX_TEST_CELL=/path/to/test-cell \
go test -race ./internal/c2j -run TestLiveC2JExecutable -v
```

Cloud adapter tests use HTTP/CLI doubles. Real cloud IAM, networking, and workload execution require deployment acceptance checks.

## Design and release documentation

- [Architecture and scheduling](DESIGN.md)
- [Provider operations](docs/providers.md)
- [Implementing a remote provider](docs/implementing-a-remote-provider.md)
- [Remote protocol](REMOTE_PROVIDER_PROTOCOL.md) and [OpenAPI schema](api/provider.openapi.yaml)
- [Docker capacity design](LOCAL_DOCKER_PROVIDER.md)
- [Release process and publishing setup](docs/releases.md)
- [c2j execution tracking guide](GUIDE-Execution-Tracking.md)

## License

[Apache-2.0](LICENSE). The npm packaging follows [c2j's release implementation](https://github.com/colony-2/c2j/tree/main/npm/c2j), with Cortex-specific installation and process-control tests.

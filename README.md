# Cortex

Cortex supplies container compute for ready c2j jobs. It polls explicitly configured repository cells, submits batches through priority tiers, and rotates first choice among equally preferred services. c2j remains an executable dependency and owns job leases, replay, and resource requirement changes.

Cortex has no database. Cooldowns and round-robin cursors live in memory. The Docker adapter reconstructs committed capacity from labeled containers.

## Build and test

Requires Linux and Go 1.24 or newer:

```sh
make build
make test
make vet
```

This builds `bin/cortex` and the static `bin/cortex-exec` timeout supervisor. The test suite covers scheduler concurrency/fallback, the remote HTTP contract, Docker admission/recovery against a fake daemon, cloud request mappings, and a complete CLI-to-remote-provider flow.

Install a compatible c2j executable separately on the controller and in executor images. The integration fixture and live discovery check were verified with c2j `v0.0.52`, source commit `5a2b646395a29ed80af4a08935fef1095838564f`. The older `v0.0.50` binary lacks the required execution flags. Pin your build using `c2j.expected_version`, which matches the complete output of `c2j version`.

Cortex does not import c2j or JobDB libraries. It runs `c2j list --json` and builds targeted container commands using `c2j run --job-id`, with actual allocation in `C2J_EXECUTION_*` environment variables.

## Run

Start with [the remote-provider example](examples/remote.yaml) or [the Docker example](examples/docker.yaml), replace the deployment values, and supply any referenced credentials through environment variables.

```sh
bin/cortex -config cortex.yaml -check
bin/cortex -config cortex.yaml -once
bin/cortex -config cortex.yaml
```

`-check` validates configuration, checks the c2j executable contract, and initializes providers; Docker initialization inspects the local daemon and acquires its admission lock. It does not launch jobs. `-once` performs one bounded polling pass. Logs are JSON on stderr. SIGINT/SIGTERM stops the controller; existing containers continue under their own lifetime controls.

Each target needs a stable `instance_id`, a tenant-selecting JobDB URL, an explicit cell list, and named launch services with positive numeric priorities. Smaller priorities are tried first. Equal-priority services rotate first choice **per batch**, and declined items visit the rest of that tier before falling through. A batch contains at most 100 jobs. `no_capacity`, `unsupported`, and confirmed `unavailable` results permit fallback. Uncertain submissions retain the cooldown without immediate fallback.

Only recipe job routes are selected by default. Configure `defaults.routes` with exact `job_type`/`task_type` values for additional automated routes that your executor images actually support. Human-input routes should be handled by their input service. Cell selectors must match the repository metadata used at submission: a local-path cell and its Git remote selector need not be interchangeable.

## Providers

| Configuration type | Implementation and prerequisites |
| --- | --- |
| `remote` | Authenticated HTTPS implementing [protocol v1](REMOTE_PROVIDER_PROTOCOL.md). Uses `endpoint` and `token_env`. `allow_http` is an explicit development option. |
| `docker` | Local Linux Docker Engine over a Unix socket, with CPU/memory/swap enforcement and an installed supervisor executable. One admission owner per daemon. |
| `cloudrun` | Cloud Run Jobs REST v2. Requires `project`, `region`, and optionally `service_account`. Authentication uses `token_env` or `gcloud auth application-default print-access-token`. |
| `ecs` | Standalone Fargate tasks via AWS CLI v2. Requires `region`, `cluster`, `subnets`, `execution_role`, and `supervisor_path`; optional `task_role`, `security_groups`, and `public_ip`. Uses the CLI's configured credentials. |
| `azurejobs` | Manual Container Apps Jobs REST API `2025-07-01`. Requires `subscription`, `resource_group`, `environment_id`, and `region`. Authentication uses `token_env` or `az account get-access-token`. |

These are built-in adapters; only `remote` requires a separate provider service. A future external-runner registry/dispatcher can implement that protocol without adding runner registration or long polling to Cortex.

### Local Docker capacity

Configure CPU, memory, and container-count budgets after reserving room for the host. Admission charges the full committed allocations, including pending and uncertain starts. Idle resource usage does not create more capacity. The adapter serializes admission, verifies created container limits before starting, and rebuilds its accounting from Docker after a controller restart.

Scratch uses a bounded tmpfs at `/scratch` by default. For `M` usable execution memory, `S` scratch, and configured overhead `H`, host admission charges `M + S + H`. The container memory limit is `M + S`; c2j receives `M` and `S` separately. There are **no disk quotas or disk-backed scratch guarantees**. The adapter uses Docker's native platform and binds each launch to a locally resolved image ID. Docker CPU/memory limits are described in [Docker's resource documentation](https://docs.docker.com/engine/containers/resource_constraints/).

Install the static `cortex-exec` helper at a path visible both to Cortex and to the Docker daemon. The adapter mounts it into the requested image and uses it to enforce deadlines and forward termination signals. Container images must contain a compatible `c2j` and support the requested process and writable scratch path. Other clients must not restart or change managed containers outside this accounting. When several controllers need a host, put a single admission service behind the remote protocol.

### Cloud allocations and images

Cloud adapters prepare supported sizes before building executor environments:

- Cloud Run supports the adapter's `linux/amd64` profile, rounds CPU/memory upward, and adds scratch to total container memory. Scratch is a bounded in-memory volume. See [memory combinations](https://docs.cloud.google.com/run/docs/configuring/jobs/memory-limits) and [in-memory volumes](https://docs.cloud.google.com/run/docs/configuring/jobs/in-memory-volume-mounts).
- ECS uses Fargate Linux platform `1.4.0`, supports `amd64` and `arm64`, and chooses valid task CPU/memory combinations. Usable scratch excludes image storage. See [Fargate storage accounting](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-task-storage.html).
- Azure uses consumption CPU/memory pairs and the associated ephemeral-storage capacity. It defaults to at most 2 vCPUs; set `max_azure_cpu_millis: 4000` only for an environment supporting those larger pairs. See [container resource combinations](https://learn.microsoft.com/en-us/azure/container-apps/containers) and [ephemeral storage](https://learn.microsoft.com/en-us/azure/container-apps/storage-mounts).

ECS and Azure require a deployment-supplied `image_storage_bounds` map for each **digest-pinned** image they can launch. Values are conservative, validated bounds for the image and provider overhead consuming task storage. Cortex subtracts that bound from provisioned storage before advertising scratch. Unknown bounds or mutable image tags return `unsupported`; a configured number is an operator assertion, not an image measurement performed by Cortex. See [cloud configuration examples](examples/clouds.yaml).

ECS images also need `cortex-exec` at the configured `supervisor_path`, since ECS standalone tasks do not supply the execution timeout used here. Cloud Run and Azure use native job timeouts and disable provider execution retries. This initial cloud implementation does not enforce queued start deadlines or working-directory overrides outside ECS; requests for those options decline rather than silently dropping them. Configure image access and workload credentials in the target deployment; per-job Azure registry/identity customization is not currently exposed.

## Inspection and retention

Every launch carries the complete jobdb instance/tenant/job/launch correlation envelope. Docker stores it in labels; Cloud Run stores it in annotations and container environment; ECS stores tags/environment; Azure stores tags/environment; remote services retain it on their launch records. The process environment also exposes `CORTEX_*` identity values.

Containers, per-launch cloud jobs, and ECS task-definition revisions are retained for inspection. Automated pruning is not implemented. Use provider tools to remove confirmed terminal resources after your retention period, preserving any parent records needed to identify retained executions. Abandoned Docker `created` containers remain charged until their outcome is inspected and they are safely removed. Host disk availability, image cache size, and retained logs remain operational concerns; there is no disk-capacity guarantee from Docker admission.

A restart loses Cortex's cooldown and may cause duplicate compute. JobDB leases protect job ownership; application side effects still need their normal idempotency. Provider start failures after ambiguous responses can also duplicate compute on a later cooldown attempt.

## Verification status

Verified during implementation:

- `go test -race ./...`, `go vet ./...`, and both executable builds.
- OpenAPI schema validation, examples, and negative cases.
- Real c2j `v0.0.52` discovery against a temporary in-memory JobDB server.
- CLI-to-remote-provider flow using an executable fixture and a local HTTP test server.
- Docker lifecycle/capacity tests against a fake Engine API; cloud tests against HTTP/CLI doubles.

No real Docker daemon or live cloud deployment was available, so real image pulls, container startup, cloud IAM/networking, and cloud execution remain deployment acceptance checks. The example runner Dockerfile is supplied for those checks but was not built here.

To repeat the live, read-only c2j contract check against a seeded test tenant:

```sh
CORTEX_TEST_C2J=/path/to/c2j \
CORTEX_TEST_JOBDB=http://127.0.0.1:8080/test-tenant \
CORTEX_TEST_CELL=/path/to/test-cell \
go test -race ./internal/c2j -run TestLiveC2JExecutable -v
```

See [DESIGN.md](DESIGN.md) for the architecture, [the Docker plan](LOCAL_DOCKER_PROVIDER.md), and [the remote OpenAPI contract](api/provider.openapi.yaml).

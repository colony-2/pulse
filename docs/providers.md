# Providers and operations

## Providers

| Configuration type | Implementation and prerequisites |
| --- | --- |
| `remote` | Authenticated HTTPS implementing [protocol v1](../REMOTE_PROVIDER_PROTOCOL.md). Uses `endpoint` and `token_env`. `allow_http` is an explicit development option. |
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

ECS and Azure require a deployment-supplied `image_storage_bounds` map for each **digest-pinned** image they can launch. Values are conservative, validated bounds for the image and provider overhead consuming task storage. Cortex subtracts that bound from provisioned storage before advertising scratch. Unknown bounds or mutable image tags return `unsupported`; a configured number is an operator assertion, not an image measurement performed by Cortex. See [cloud configuration examples](../examples/clouds.yaml).

ECS images also need `cortex-exec` at the configured `supervisor_path`, since ECS standalone tasks do not supply the execution timeout used here. Cloud Run and Azure use native job timeouts and disable provider execution retries. This initial cloud implementation does not enforce queued start deadlines or working-directory overrides outside ECS; requests for those options decline rather than silently dropping them. Configure image access and workload credentials in the target deployment; per-job Azure registry/identity customization is not currently exposed.

## Inspection and retention

Every launch carries the complete jobdb instance/tenant/job/launch correlation envelope. Docker stores it in labels; Cloud Run stores it in annotations and container environment; ECS stores tags/environment; Azure stores tags/environment; remote services retain it on their launch records. The process environment also exposes `CORTEX_*` identity values.

Containers, per-launch cloud jobs, and ECS task-definition revisions are retained for inspection. Automated pruning is not implemented. Use provider tools to remove confirmed terminal resources after your retention period, preserving any parent records needed to identify retained executions. Abandoned Docker `created` containers remain charged until their outcome is inspected and they are safely removed. Host disk availability, image cache size, and retained logs remain operational concerns; there is no disk-capacity guarantee from Docker admission.

A restart loses Cortex's cooldown and may cause duplicate compute. JobDB leases protect job ownership; application side effects still need their normal idempotency. Provider start failures after ambiguous responses can also duplicate compute on a later cooldown attempt.


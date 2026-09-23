# Providers and operations

## Providers

| Configuration type | Implementation and prerequisites |
| --- | --- |
| `remote` | Authenticated HTTPS implementing [protocol v1](../REMOTE_PROVIDER_PROTOCOL.md). Uses `endpoint` and `token_env`. `allow_http` is an explicit development option. |
| `docker` | Local Linux Docker Engine over a Unix socket, with CPU/memory/swap enforcement and an installed supervisor executable. One admission owner per daemon. |
| `cloudrun` | Cloud Run Jobs REST v2. Requires `project`, `region`, and optionally `service_account`. Uses native Google Application Default Credentials (attached service account, credential file, or federation). Optional `token_env` overrides discovery. |
| `ecs` | Standalone Fargate tasks via the AWS SDK for Go v2. Requires `region`, `cluster`, `subnets`, `execution_role`, and `supervisor_path`; optional `task_role`, `security_groups`, and `public_ip`. Uses the AWS credential chain, including environment credentials and task/instance roles. |
| `azurejobs` | Manual Container Apps Jobs REST API `2025-07-01`. Requires `subscription`, `resource_group`, `environment_id`, and `region`. Uses native Azure environment, workload-identity, or managed-identity credentials. Optional `token_env` overrides discovery. |

These are built-in adapters; only `remote` requires a separate provider service. A future external-runner registry/dispatcher can implement that protocol without adding runner registration or long polling to Cortex.

All cloud adapters work in the default distroless image. See [cloud authentication](cloud-authentication.md) for environment variables, mounted credential files, identity permissions, and refresh behavior.

### Local Docker capacity

Configure CPU, memory, and container-count budgets after reserving room for the host. Admission charges the full committed allocations, including pending and uncertain starts. Idle resource usage does not create more capacity. The adapter serializes admission, verifies created container limits before starting, and rebuilds its accounting from Docker after a controller restart.

Scratch uses a bounded tmpfs at `/scratch` by default. For `M` usable execution memory, `S` scratch, and configured overhead `H`, host admission charges `M + S + H`. The container memory limit is `M + S`; c2j receives the requested memory and scratch separately, even when native limits round upward. There are **no disk quotas or disk-backed scratch guarantees**. The adapter uses Docker's native platform and binds each launch to a locally resolved image ID. Docker CPU/memory limits are described in [Docker's resource documentation](https://docs.docker.com/engine/containers/resource_constraints/).

Install the static `cortex-exec` helper at a path visible both to Cortex and to the Docker daemon. The adapter mounts it into the requested image and uses it to enforce deadlines and forward termination signals. Container images must contain a compatible `c2j` and support the requested process and writable scratch path. Other clients must not restart or change managed containers outside this accounting. When several controllers need a host, put a single admission service behind the remote protocol.

### Cloud allocations and images

Cloud adapters select supported sizes internally during submission. Cortex builds executor environments from requested/defaulted resources and providers preserve them, even when native allocations round upward:

- Cloud Run supports the adapter's `linux/amd64` profile, rounds CPU/memory upward, and adds scratch to total container memory. Scratch is a bounded in-memory volume. See [memory combinations](https://docs.cloud.google.com/run/docs/configuring/jobs/memory-limits) and [in-memory volumes](https://docs.cloud.google.com/run/docs/configuring/jobs/in-memory-volume-mounts).
- ECS uses Fargate Linux platform `1.4.0`, supports `amd64` and `arm64`, and chooses valid task CPU/memory combinations. Usable scratch excludes image storage. See [Fargate storage accounting](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-task-storage.html).
- Azure uses consumption CPU/memory pairs and the associated ephemeral-storage capacity. It defaults to at most 2 vCPUs; set `max_azure_cpu_millis: 4000` only for an environment supporting those larger pairs. See [container resource combinations](https://learn.microsoft.com/en-us/azure/container-apps/containers) and [ephemeral storage](https://learn.microsoft.com/en-us/azure/container-apps/storage-mounts).

ECS and Azure require a deployment-supplied `image_storage_bounds` map for each **digest-pinned** image they can launch. Values are conservative, validated bounds for the image and provider overhead consuming task storage. The adapter subtracts that bound from provisioned storage to ensure it can guarantee the requested scratch. Unknown bounds or mutable image tags return `unsupported`; a configured number is an operator assertion, not an image measurement performed by Cortex. See [cloud configuration examples](../examples/clouds.yaml).

ECS images also need `cortex-exec` at the configured `supervisor_path`, since ECS standalone tasks do not supply the execution timeout used here. Cloud Run and Azure use native job timeouts and disable provider execution retries. This initial cloud implementation does not enforce queued start deadlines or working-directory overrides outside ECS; requests for those options decline rather than silently dropping them. Configure image access and workload credentials in the target deployment; per-job Azure registry/identity customization is not currently exposed.

## Submission attempts

Cortex calls each selected launch service once per attempt and does not retry uncertain submissions. Providers perform one native launch attempt; optional native idempotency safeguards do not imply a replay or recovery requirement. If work is lost or fails to start, c2j/JobDB readiness and Cortex's cooldown govern a fresh attempt with a new ID. `accepted` reports admission or handoff, not guaranteed execution. The protocol imposes no durable submission journal, stored decline decisions, or fixed terminal retention. Built-in safeguards such as Docker's deterministic names and capacity reconstruction remain implementation choices.

Prefer prompt handoff to native compute. Deliberate provider-owned runner queues require a startup deadline; native scheduling and image-pull delays alone do not. When `start_before` is explicitly supplied, its actual-start constraint must still be enforced or declined as unsupported. See [submission and recovery semantics](../REMOTE_PROVIDER_PROTOCOL.md#submission-attempts-and-recovery).

## Active instances and retention

Every launch carries the complete jobdb instance/tenant/job/launch correlation envelope. Docker stores it in labels; Cloud Run stores it in annotations and container environment; ECS stores tags/environment; Azure stores tags/environment; remote services attach it to native resources or provider-owned queue entries. The process environment also exposes `CORTEX_*` identity values.

The [public HTTP API](http-api.md) lists active instances through every configured provider. Lists include queued/starting/running work and exclude terminal instances; paused/stopping compute is included where reported. Only Cortex-managed native resources are returned, within each configured provider's scope. A service can show launches made before Cortex restarted or by another controller using the same scope. Duplicate configurations pointing at the same native scope can show the same resource under both provider names.

| Provider | Listing source and scope |
| --- | --- |
| Remote | `GET /v1/launches`, scoped to the provider endpoint and authenticated principal. Includes accepted queue entries before native assignment. |
| Docker | Labeled containers on the configured daemon. Created/restarting containers are `starting`; running/paused/removing map to `running`/`paused`/`stopping`. Exited/dead containers are excluded. Reads do not update admission accounting. |
| Cloud Run | Executions across jobs in the configured project/region. Pending executions are `starting`; a positive running task count is `running`. Completion/deletion timestamps or terminal completion conditions exclude an execution. Requires `run.executions.list`. |
| ECS | `ListTasks` for the configured cluster with desired status `RUNNING`, followed by `DescribeTasks` including tags. Provisioning/pending/activating tasks are `starting`; running tasks are `running`. Stopped tasks are excluded. Requires `ecs:ListTasks` and `ecs:DescribeTasks`. |
| Azure Jobs | Jobs in the configured resource group, then execution pages for managed jobs. Processing executions are `starting`, running executions are `running`; terminal statuses or end times are excluded. Requires read access to jobs and their executions. |

Cloud APIs can briefly lag acceptance or state changes. In particular, an accepted native start operation may not yet have an execution to list. Cloud Run and ECS pages can be empty after filtering while still carrying a continuation token. Azure walks nested job/execution pages with a stateless cursor; large inventories may need several requests. Docker sorts current container IDs and uses a last-seen-ID cursor. These are changing views, not historical or transactional snapshots. An empty list does not prove a past submission never ran or free capacity is available.

Containers, per-launch cloud jobs, and ECS task-definition revisions are retained for inspection. Automated pruning is not implemented. Use provider tools to remove confirmed terminal resources after your retention period, preserving any parent records needed to identify retained executions. Abandoned Docker `created` containers remain charged until their outcome is inspected and they are safely removed. Host disk availability, image cache size, and retained logs remain operational concerns; there is no disk-capacity guarantee from Docker admission.

A restart loses Cortex's cooldown and may cause duplicate compute. JobDB leases protect job ownership; application side effects still need their normal idempotency. Provider start failures after ambiguous responses can also duplicate compute on a later cooldown attempt.


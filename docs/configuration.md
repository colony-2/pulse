# Configuration and container deployment

**Pulse runs without a Pulse configuration file.** It uses c2j tenant settings and Docker by default. Optional YAML overrides defaults or configures additional providers and tenants. `PULSE_CONFIG` contains YAML directly, using the same schema and validation as files, including in the distroless image.

## Source selection

| Priority | Source |
| --- | --- |
| 1 | File explicitly selected by `--config PATH`. |
| 2 | YAML contents in `PULSE_CONFIG`, if the variable is set. |
| 3 | `pulse.yaml` in the working directory, if present. |
| 4 | Built-in defaults. |

Only one source is used. There is no merging, and a selected source's failure does not trigger fallback. A set-but-empty or whitespace-only `PULSE_CONFIG` is an error; unset it to use the local file or built-in defaults. An explicit file ignores the inline value, even if that value is invalid. An explicit missing file fails instead of using the environment.

The value is raw YAML, not a filename, URL, or base64 string. Use real newlines for multiline YAML. Pulse performs no shell expansion or `${VARIABLE}` interpolation. Quoting and expansion performed by your host shell or deployment tooling happen before Pulse receives the value.

Configuration is read once at startup. Restart or deploy a new revision after changes. `check` and `run --once` use the same source selection; `version` needs no configuration. `/config` shows the parsed, redacted configuration regardless of its source, never the raw environment document. See [HTTP API](http-api.md).

## Tenant selection and defaults

Pulse selects the tenant using `--jobdb`, an explicit Pulse target's `jobdb`, `C2J_JOBDB`, then `jobdb` in the nearest ancestor `.c2j/config.yaml`. The c2j public settings loader handles both literal and command-valued project settings. No repository scope is needed: discovery lists all eligible recipe jobs in the tenant, across repositories. Remove obsolete target `cells` and top-level `c2j` settings when upgrading; validation rejects them.

```sh
export C2J_JOBDB=https://jobdb.example.com/acme
pulse
# Equivalent continuous command:
pulse run
pulse run --once
pulse check
pulse version
```

Pulse needs an HTTP(S) JobDB service with a tenant path. A missing tenant produces an error explaining these settings; c2j's `embed:///` database cannot serve remote container workers. Use `jobdb_token_env` on a target when the service needs a bearer token. Multiple targets remain supported through YAML; `--jobdb` can override only a single target. Missing `instance_id` values are stable hashes of the deployment URL, shared by tenants on that deployment.

| Setting | Default |
| --- | --- |
| Provider | Local Docker, discovered using the active Docker context/socket |
| Image | `ghcr.io/colony-2/base:latest` |
| CPU / memory / scratch | `1` / `1Gi` / `1Gi` |
| Platform | Docker daemon's native Linux platform for a single Docker provider; otherwise `linux/<host architecture>` |
| Execution timeout | Managed by c2j; no Pulse cap |
| Docker capacity | One default-sized job plus `256Mi` memory overhead |
| `max_jobs_per_tenant` | `100` per poll |
| `max_pages_per_tenant` | `1000` per poll |

Job execution requirements override allocation defaults. `defaults` in optional YAML changes the fallback values. When providers are explicitly configured, targets specify their `launch_services`. For advanced deployments, keep `instance_id` explicit if it must survive changes to the deployment URL. Executor images still provide c2j and recipe dependencies.

## Execution lifetime

c2j enforces recipe and operation timeouts, cancels process trees, and stops work on lease-renewal failure. Pulse no longer adds a one-hour timer to every container. Docker runs c2j directly and sends the lease through native stdin; no helper or host bind mount is required. Remove obsolete Docker `helper` settings when upgrading.

`defaults.timeout`, if set, is an explicit infrastructure lifetime cap, from one second to one year. Providers must enforce a requested cap or decline it. Docker declines positive caps; use c2j recipe timeouts. Cloud Run and Azure require an explicit cap because their native job APIs require a task timeout. ECS and remote providers can use executor-managed timeouts without a Pulse cap. Cloud adapters still use `pulse-exec` as a stdin bridge; with no cap it applies no timer.

Remote provider protocol 1.2 makes `timeout_seconds` optional: absent or zero means executor-managed timeouts. Upgrade remote providers before sending requests without a cap.

## Lease handoff and recovery

Pulse acquires an ordinary JobDB lease for the selected job before asking a provider to start compute. It exports the capability using JobDB's public API and supplies it to `c2j run with-lease --job-id … --lease-file -`. c2j validates and renews that exact lease before running work. It cannot claim a replacement job. Pulse does not heartbeat after acquisition, including during provisioning.

```yaml
poll_interval: 5s
lease_duration: 5m
claim_concurrency: 8
claim_timeout: 5s
batch_size: 100
cooldown: 60s
call_timeout: 10s
batch_timeout: 1m
```

`lease_duration` defaults to `5m` and accepts `1s` through `24h`. Allow enough time for lease acquisition of the batch, provider fallback, scheduling, image pulls, and c2j startup. A longer duration also delays recovery after a lost launch or runner crash; c2j renews with the inherited duration. `defaults.start_window`, when present, must be positive and no longer than `lease_duration`. It constrains actual startup; it does not extend the lease. Docker declines this option because it launches c2j directly; c2j validates the supplied lease on startup.

Claims run concurrently before provider submission, with these controls:

| Setting | Default | Meaning |
| --- | --- | --- |
| `claim_concurrency` | `8` | Maximum simultaneous claim/preparation calls per batch; range 1–100. Actual concurrency is also bounded by the candidate count. |
| `claim_timeout` | `5s` | How long to keep starting claims for a batch. In-flight calls finish normally under `call_timeout` and the remaining `batch_timeout`. |
| `batch_size` | `100` | Maximum candidate claim attempts and therefore maximum acquired leases per provider batch; range 1–100. Failed or unavailable claims count toward this limit. |

Pulse submits as soon as every candidate has been processed, without waiting out the window. Once the time or count limit is met, it stops starting claims, lets outstanding calls finish, and submits the successful subset. Unstarted candidates remain eligible for a later poll and receive no failure cooldown. Attempted claims that fail follow normal backoff; a timed-out claim whose capability was never returned recovers through JobDB lease expiry. A returned but unusable lease is released safely. Fallback reuses the already acquired leases.

Leave room in `batch_timeout` for provider submission after the claim phase, and in `lease_duration` for claiming plus container startup. Increasing the count or concurrency raises instantaneous JobDB load; increasing the claim window consumes more of the earliest acquired leases' lifetime. The claim window does not cancel requests, so the claim phase can extend beyond it while in-flight calls finish.

`cooldown` is provisioning-failure backoff. After definite non-start, Pulse releases the unused lease by rescheduling the same route with a wait deadline, preserving task coordinates and client payload. JobDB persists that wait across controller restarts. Pulse also keeps local failure backoff, including claim/preparation failures. Accepted and uncertain submissions have no local cooldown: they retain their lease until c2j releases it or it expires. A successful executor yield can therefore be scheduled on the next poll without waiting for an old cooldown.

The configured `jobdb_token_env` authorizes discovery and claiming on the controller. It is not copied into the job container. Use JobDB v0.0.22 or later with authoritative supplied-lease renewal, a c2j image supporting `run with-lease`, and a provider supporting sensitive stdin. No activation API or lease transfer transaction is required. See [provider image and transport requirements](providers.md).

## Docker dependency caches

The Docker provider automatically creates shared dependency volumes for
`ghcr.io/colony-2/base` images, including tags and pinned digests. No host Nix,
manual volume setup, or daemon is required. Other images run without these mounts.
Optional overrides:

```yaml
providers:
  docker:
    type: docker
    dependency_cache:
      enabled: true
      namespace: default
      generation: "1"
      max_age: 720h
```

These are the defaults. Set `enabled: false` to use container-local stores.
Change `generation` to get fresh stores. Cache identity also includes the JobDB
instance, tenant, resolved image, and native platform. Docker aliases for one
socket must agree on cache policy. `max_age` accepts positive Go durations up to
`8760h`; it measures age since initialization, not time since last use.

Pulse retains c2j tools, uv downloads, and pnpm's store under `/var/cache/pulse`,
and the complete image-seeded `/nix` tree in a second volume. Workers use direct
local Nix access with automatic GC disabled. The image-provided Python is used;
automatic interpreter downloads are disabled. Conflicting cache environment
settings and scratch paths overlapping these mounts are rejected.

A short-lived initializer validates a new pair before worker launch. Its stopped
container records readiness; it consumes no running capacity. Pulse checks for
expired pairs at startup and at most hourly during polling, deleting only pairs
with no worker references and no uncertain creates. Active workers continue to
use their stores when Pulse stops. Cleanup resumes when Pulse restarts with
caching enabled and the same operator namespace. Volumes have no disk quota.

Shared writable executable caches assume jobs within a tenant trust each other;
disable sharing for mutually untrusted jobs. Workers must not run Nix GC or delete
shared store contents. See the [cache design](../PROPOSAL_DOCKER_DEPENDENCY_CACHES.md)
for failure recovery and image prerequisites. In particular, the reviewed base
image's c2j `0.0.63` predates addons: addon jobs require a newer worker in that image.

## Docker or a native executable

Copy [container.yaml](../examples/container.yaml), edit the deployment settings, then load its contents on the host:

```sh
export PULSE_CONFIG="$(cat pulse.yaml)"
pulse check
pulse
```

For Docker, pass the environment variable by name:

```sh
docker run --rm --init --read-only --tmpfs /tmp -p 8080:8080 \
  -e PULSE_CONFIG -e PULSE_PROVIDER_TOKEN \
  ghcr.io/colony-2/pulse:latest
```

The host file is just an authoring convenience. You can instead store the entire YAML directly in your deployment's environment settings. The container needs no configuration mount, shell, or startup script. Credentials remain in separate secret/environment settings or cloud identities; fields such as `token_env` and `jobdb_token_env` name those variables.

For example, a shell can set the document directly using single quotes, which preserve literal dollar signs and newlines:

```sh
export PULSE_CONFIG='
defaults:
  image: registry.example/c2j-runner:1.0
  platform: linux/amd64
  cpu: "1"
  memory: 1Gi
  scratch: 1Gi
providers:
  runners:
    type: remote
    endpoint: https://runners.example.com
    token_env: PULSE_PROVIDER_TOKEN
targets:
  - instance_id: production
    jobdb: https://jobdb.example.com/acme
    launch_services: [{name: runners, priority: 1}]
'
```

## Google Cloud Run

For a simple Cloud Run service deployment, add `PULSE_CONFIG` under **Containers → Variables & Secrets**, with the complete YAML as its value. Leave container arguments unset so an explicit `--config` does not override it. Environment settings belong to the service revision. Cloud Run limits each environment variable to 32 KB; larger configurations can use a mounted file. See [Cloud Run environment configuration](https://docs.cloud.google.com/run/docs/configuring/services/environment-variables).

If you already store the document in Secret Manager, Cloud Run can inject a selected version as the `PULSE_CONFIG` environment variable instead of mounting a file. See [secret injection](https://docs.cloud.google.com/run/docs/configuring/services/secrets). Use the controller's attached service account for cloud API access as described in [cloud authentication](cloud-authentication.md).

Pulse's continuous polling needs **instance-based billing and at least one minimum instance** so CPU remains available without HTTP traffic. See [background processing settings](https://docs.cloud.google.com/run/docs/configuring/billing-settings). Leave `http.listen` unset to use Cloud Run's `PORT` value.

## Mounted-file alternative

File configuration remains available for larger documents and deployments that already manage mounts:

```sh
docker run --rm --init --read-only --tmpfs /tmp -p 8080:8080 \
  --mount "type=bind,source=$PWD/pulse.yaml,target=/etc/pulse/pulse.yaml,readonly" \
  -e PULSE_PROVIDER_TOKEN \
  ghcr.io/colony-2/pulse:latest --config /etc/pulse/pulse.yaml
```

The file must be readable by UID/GID `65532:65532`. The image no longer supplies `--config /etc/pulse/pulse.yaml` as default arguments, so existing deployments using that mounted path must add the explicit arguments shown above. No source is baked into the image; environment-only deployments start it without arguments.

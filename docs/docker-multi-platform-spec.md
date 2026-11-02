# Docker provider: multiple execution platforms

Status: Implemented

## Problem

Previously, Pulse derived one platform from the Docker daemon and rejected requests
for any other architecture. Docker Desktop supports running other architectures
through emulation, so an Apple Silicon machine can run both Linux arm64 and
Linux amd64 containers. Pulse needs to select the correct image throughout the
launch path to use this capability safely.

Pulse supports **one Docker provider with automatically detected
execution platforms and a shared capacity budget**, without new configuration.

## Convention: detect executable platforms automatically

Existing Docker configuration is sufficient:

```yaml
providers:
  local:
    type: docker
    capacity:
      cpu: "4"
      memory: 8Gi
      max_containers: 4
```

At startup, discover the daemon's native platform and probe additional candidate
platforms. Enable each additional platform only after a successful execution
probe. On an arm64 Docker daemon with working amd64 emulation, Pulse automatically
accepts both `linux/arm64` and `linux/amd64` jobs.

The initial candidate set is `linux/arm64` and `linux/amd64`, excluding the native
platform. Preserve existing support for other native Linux platforms. This is a
bounded discovery policy, not a claim to enumerate every possible architecture.

There is no `platforms` setting in the initial implementation. Keep
`defaults.platform` as the existing way to choose a platform for jobs that omit
one; it does not restrict the provider's detected capabilities.

This intentionally expands admission on capable daemons. Jobs without an
explicit platform continue to default to the native architecture.

## Execution capability validation

Do not infer support from the controller running on macOS or Apple Silicon. The
selected Docker socket determines where containers run.

The daemon's reported architecture identifies its native platform, not the full
set it can execute. Buildx platform lists describe a builder, which may use a
different runtime or remote nodes. Neither is sufficient proof that ordinary
containers on the selected daemon can execute a particular architecture.

At startup, and during `pulse check`, probe each non-native candidate:

1. Resolve the digest-pinned Docker Official `hello-world` Linux image for that
   exact platform with the common platform-aware resolver. It consists of a
   single executable on `scratch`; Docker reports approximately 15 KB per variant.
   Verify image OS and architecture before executing it. Resource-limit probes
   use the same image.
2. Create a container with an explicit platform and run `/hello`.
3. Wait for completion and require exit code zero. Successful creation or
   startup alone is insufficient.
4. Bound each candidate attempt to 30 seconds and half the remaining
   initialization deadline supplied by `call_timeout`. Bound start and wait
   together to ten seconds. Use no host mounts, privileges, or container
   networking. Disable container logging and attempt cleanup with a separate
   ten-second context.
5. Add the platform to the supported set only on success. Record unsuccessful
   probes as either unavailable execution support or inconclusive discovery,
   with a diagnostic explaining the stage that failed.

Reuse locally cached probe variants. Pull only when the required variant is
absent; image pulling can require registry network access even though the probe
container has networking disabled. A pull/authentication failure or timeout does
not prove that emulation is unavailable. Mark that result inconclusive and leave
the platform unadvertised for this provider lifetime.

Failure to verify an optional platform does not prevent native execution or
make `check` fail by itself. Preserve existing failures for daemon initialization
and native resource-limit checks. Cancellation of provider initialization still
aborts startup.

Cache results in memory per provider instance, sharing them across aliases.
Reprobe on restart and on a fresh `check`; do not persist results across daemon
lifetimes or probe on every submission. Runtime refresh is outside the initial
scope. If Docker's emulation settings change, restart Pulse to rediscover them.

Keep architecture validation separate from resource-limit validation. Disabling
CPU or memory enforcement must not bypass the architecture probe. Pulse uses the
daemon's existing emulation setup; it does not install emulators or change
Docker Desktop settings.

## Supported platforms and default selection

Replace the provider's single platform concept with:

```go
NativePlatform() string
SupportedPlatforms() []string
```

A job with an explicit platform must run on exactly that platform. A job without
one continues to use `defaults.platform`, or the daemon's native platform under
the existing automatic-default behavior.

Successful discovery never changes the automatic default from the native
platform. An explicit `defaults.platform` remains authoritative; if a Docker
provider cannot execute it, that provider declines the request through the
existing fallback flow. Other configured providers may still satisfy it.

Both architectures can be submitted to the same provider. There is no
architecture rewriting or automatic retry on another architecture. The existing
request already carries a platform, so this requires no provider-protocol or
JobDB route change.

## Image resolution and container creation

Currently, pulls specify a platform, but image inspection uses only the image
reference and container creation omits the platform. Removing the native-only
admission check is therefore insufficient.

Introduce one image-resolution helper used by workers, startup probes, and
dependency-cache initializers:

1. Resolve by **image reference and requested platform**.
2. Inspect that platform explicitly.
3. Pull the requested variant when it is missing, including when another variant
   of the same tag is already cached.
4. Verify the resolved image's OS and architecture.
5. Preserve digest-pin verification.
6. Bind the launch to the verified immutable local root image ID. For an image
   index, inspect the requested platform through that root ID and preserve its
   selected configuration. A child manifest ID returned by platform inspection
   is not necessarily independently addressable by Docker create. The root ID
   and explicit platform together identify the launch content without rereading
   a mutable tag.
7. Pass the requested platform explicitly to container creation.

Alternating arm64 and amd64 jobs using the same image tag must work without
choosing whichever variant was last pulled.

For the initial implementation, require **Docker Engine API 1.49 or later for
non-native execution**. That version introduced platform-specific image
inspection. Older daemons retain native-only operation and log that additional
platform discovery was skipped due to the API version. This is an implementation
prerequisite, not evidence that those daemons lack emulation. It avoids building
ambiguous image-resolution fallback behavior into the first release.

The image-inspection API uses a JSON-encoded OCI platform query value; use the
encoding documented for each endpoint rather than assuming inspection, pull,
and container creation take identical query values.

## Capacity and dependency caches

Both platforms consume the same CPU, memory, scratch, and container-count
budgets. With `max_containers: 4`, two arm64 and two amd64 containers fill the
pool.

Preserve the single admission lock, uncertain-launch accounting, and restart
reconstruction. Docker aliases sharing a daemon share the same provider instance
and detected capability set, while retaining the existing requirement for
identical budgets and configuration.

Dependency-cache keys already include the requested platform. Preserve that
separation and ensure each cache initializer runs with its worker's platform.

Do not introduce an arbitrary emulation CPU multiplier. Existing resource
accounting measures allocations; it does not promise equivalent throughput
across architectures.

## Diagnostics and failure behavior

Startup and `check` should report the native platform, enabled platforms, and
non-native probe results. For example:

```text
Docker provider ready: native=linux/arm64 supported=[linux/arm64,linux/amd64]
```

Distinguish successful execution, execution failure, inconclusive discovery
(such as a registry timeout), and discovery skipped for an older API. Do not
claim to have identified whether Docker uses Rosetta, QEMU, or another mechanism.

Requests for a platform outside the detected set return `unsupported` before
creating a worker, with a reason distinguishing failed execution from unverified
capability where known.
Existing handling of uncertain worker starts remains unchanged. A successful
probe demonstrates basic execution capability, not compatibility with every
application or instruction-set requirement.

## Implementation scope

- `internal/providers/providers.go`: preserve native default selection and share
  discovery results through the existing provider reuse for aliases.
- `internal/providers/docker/docker.go`: represent native and supported
  platforms separately, admit enabled platforms, and create workers with an
  explicit platform.
- `internal/providers/docker/engine.go`: provide platform-aware image resolution
  and API capability checks.
- `internal/providers/docker/limits.go`: use the common image resolver for
  resource-limit probes; keep architecture validation independent.
- Add a Docker platform-discovery helper for candidate selection, execution
  probes, result classification, and cleanup.
- `internal/providers/docker/cache.go`: create initializers with the worker's
  platform and preserve platform-specific cache keys.
- Update provider documentation and Docker examples.

No new YAML fields or platform list in `docker.Config` are required. Runtime
capabilities belong to the provider instance, preserving existing configuration
comparison for aliases.

## Acceptance criteria

- Existing configurations continue to work without edits; jobs without a
  platform retain native default selection.
- Apple Silicon automatically discovers working amd64 execution and runs arm64
  and amd64 jobs concurrently.
- Discovery depends on the selected daemon, including when the controller runs
  inside a container or has a different architecture from the daemon.
- Alternating architectures under one image tag selects the correct image every
  time.
- Missing emulation leaves native execution available and excludes the failed
  candidate.
- A probe that starts but exits unsuccessfully does not enable the platform.
- Registry failures and timeouts produce inconclusive diagnostics and leave
  native execution available.
- Mixed architectures share capacity, including after restart.
- Workers and cache initializers use matching platforms.
- Architecture-specific digest mismatches decline before worker creation.
- Older APIs retain native support and report why discovery was skipped.
- Probe cleanup is attempted on success, failure, and timeout.
- Aliases share one discovery result and do not repeat probes.
- Restart and a fresh `check` rerun discovery after daemon capability changes.

## References

- [Current Docker provider implementation](../internal/providers/docker/docker.go)
- [Provider construction and default platform selection](../internal/providers/providers.go)
- [Dependency-cache implementation](../internal/providers/docker/cache.go)
- [Docker multi-platform documentation](https://docs.docker.com/build/building/multi-platform/)
- [Docker Engine API version history](https://docs.docker.com/reference/api/engine/version-history/)
- [Docker Buildx inspect](https://docs.docker.com/reference/cli/docker/buildx/inspect/)
- [Docker Official hello-world source](https://github.com/docker-library/hello-world)

Probe image: `docker.io/library/hello-world@sha256:5e22040d441e5fb3aed38368acbe8486b575d7018df38dbdfbc7311fbb2ef3a9`.
The pinned amd64 variant has a single 2,415-byte compressed layer. The pin is the
Linux multi-platform index, not one architecture's manifest.

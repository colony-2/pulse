# Contributing to Pulse

See the [README](README.md) for installation and use. This guide covers building, testing, and changing Pulse.

## Development setup

Use Go 1.26 or newer and a running Docker Engine on Linux or Docker Desktop on macOS. Packaging checks also require Node.js 22+, Python 3, and standard Unix archive tools. Docker with Buildx is needed for container smoke checks.

```sh
make build          # bin/pulse and bin/pulse-exec
make test           # Go tests with the race detector
make vet
make test-packaging # npm installer tests
```

Job discovery uses `github.com/colony-2/c2j/pkg/joblist`, pinned in `go.mod`. No c2j executable is needed to build or run the controller. Executor job images still need c2j. The integration pins c2j `v0.0.65` and JobDB `v0.0.28`; there is no local module replacement.

When updating c2j, verify the public listing projection and execution compatibility against the new dependency. Keep the dependency pinned and run `go mod tidy -diff` to check module tidiness.

## Tests and validation

The Go suite covers scheduler concurrency/fallback, supplied-lease handoff and failure cleanup, provisioning backoff, provider state filtering and pagination, Docker accounting/recovery, cloud requests/authentication, HTTP diagnostics, and configuration precedence. The library adapter talks to a test server implementing the real JobDB HTTP protocol, exercising cancellation, tenant isolation, pagination, and execution demand projection.

CLI integration tests exercise file and inline configuration with an empty `PATH`, discover a job through JobDB, and submit it through the remote protocol. Continuous-mode tests verify public HTTP access and graceful shutdown.

Validate the remote OpenAPI contract and its examples in a Python environment:

```sh
python3 -m venv /tmp/pulse-protocol-venv
/tmp/pulse-protocol-venv/bin/pip install 'openapi-spec-validator==0.9.0' 'PyYAML==6.0.3'
/tmp/pulse-protocol-venv/bin/python scripts/validate_protocol.py
```

Cloud adapter tests use HTTP doubles for native APIs and credential endpoints. Docker unit tests use a fake daemon API. On Linux and macOS, ordinary `go test ./...` also runs real Docker integration tests by default. To confirm container execution with uncached, verbose output:

```sh
go test -race -count=1 -timeout=45m -run '^TestDocker(C2J|DependencyCache)?Integration$' -v ./internal/providers
```

These tests use Pulse's core socket/context discovery. No socket override, opt-in flag, or local c2j installation is required; missing or unusable Docker fails the suite on Linux and macOS.

`TestDockerC2JIntegration` pulls `ghcr.io/colony-2/base:latest`, reports its digest and installed c2j version, and starts a real SQLite-backed JobDB HTTP server and Git HTTP repository. It invokes the native `pulse run --once` CLI with only `C2J_JOBDB`, so provider, image, platform, resources, and lease duration use production defaults. Pulse discovers and claims a submitted recipe, then hands the lease over stdin to the image's installed `c2j run with-lease`. The test checks job completion, saved output and execution artifacts, background heartbeat renewal, container-to-host access through `host.docker.internal`, persisted recipe failures, recipe timeout enforcement, and termination on initial or background renewal transport failures. The transport test drops an actual TCP connection and checks that initial lease validation fails before job execution, with no execution chapters persisted. Workers receive no host mounts or replacement binaries. The test subscribes to Docker logs and removal notifications before allowing execution to finish, then verifies automatic removal on both successful and failed jobs. Heartbeat cases wait for protocol events, including the default lease's renewal interval, rather than asserting short wall-clock deadlines.

The SQLite-backed HTTP/Git fixture runs in its own Docker container. The test
streams a static fixture binary through `docker cp`, so the daemon does not need
access to the test runner's filesystem. Native-host and Docker Desktop runners
use its published loopback port; a runner in a peer container can use a reachable
daemon gateway instead. Workers use the fixture's peer address for Git and recipe
HTTP calls. Separate cases verify peer access, host-gateway access to the published
port, and Pulse's production localhost-to-`host.docker.internal` translation.
When the runner is a peer, the localhost case forwards its controller requests
locally to the published endpoint while workers use the actual Docker host port.
The localhost case probes readiness before binding and reuses Docker's published
port on native runners, even if initial endpoint discovery selected the gateway.
Fixture control requests and native controller calls bypass HTTP proxies for
these local endpoints. Go, the Docker CLI, permission to use the daemon, and
registry access are required; the base image supplies Git. Each test cleans up
its fixture, workers, database, and cache volumes; downloaded images remain cached.

Image preparation has a separate 30-minute timeout for downloading and unpacking the large default image; each supplied-lease scenario then gets its own eight-minute budget, so one slow scenario cannot exhaust later scenarios' budgets. Fixture deletion gets a fresh two-minute deadline independent of log collection. Cache cleanup includes initializers that failed before a worker was created. CI pre-pulls the image in a separate step with visible progress, and the test still refreshes `latest` on every uncached run. For cold pulls, use `go test -race -count=1 -timeout=45m ./...` (also used by CI) to allow more than Go's default ten-minute package timeout. Plain `go test ./...` still includes these tests without opt-in; it can hit Go's own deadline on a slow first pull.

`TestDockerIntegration` separately checks process output, finite stdin and EOF, environment delivery, working directory, tmpfs and resource configuration, nonzero exits, restart accounting, capacity reuse, and active-instance listing using Alpine. Resource assertions check either enforced limits or the startup warning and recorded fallback mode. Unit tests cover both modes, socket/context selection, probe failures, cleanup, and recovery across capability changes. A slow-cache regression delays initializer creation for 31 seconds over the real Unix-socket transport and submits through the scheduler with production timeout defaults. Cancellation coverage checks that initialization releases capacity before retry. Live cloud IAM, networking, image access, resource enforcement, and workload execution still require deployment acceptance checks; report which checks you actually ran.

CI runs Go checks and real Docker jobs on Linux AMD64, Linux ARM64, and macOS Intel (using Colima for the CI daemon), cross-compiles all four release executables, tests npm installation, and smoke-tests both container architectures. See [the test workflow](.github/workflows/test.yaml).

`TestDockerDependencyCacheIntegration` builds a test-only worker using the pinned
c2j tool manager and copies it into a derived Colony base image. It exercises two
concurrent cold workers, two offline warm workers after provider restart, scoped
uv/pnpm versions, Nix tools and extension-output retention, generation changes,
and automatic volume retirement. It removes its containers, volumes, and derived
image. The upstream base image remains cached. First runs require Python/npm/Nix
registry access. This test validates storage and the tool manager; it does not
replace the supplied-lease test of the image's installed c2j binary or test the
full extension compiler/replay lifecycle. The verified base ships c2j
0.0.65, matching the pinned library. Its missing `sed` and
native Python wheel loader are documented in the [cache design](PROPOSAL_DOCKER_DEPENDENCY_CACHES.md).

## Build container images

The distroless image contains Pulse, `pulse-exec`, CA certificates, and dependency license/version records. Both executables are built with CGO disabled. The c2j listing library is compiled into Pulse; no c2j executable is downloaded.

```sh
docker buildx build --load \
  --build-arg VERSION=dev \
  -t pulse:local .
```

For a registry build covering both architectures, replace `--load` with `--platform linux/amd64,linux/arm64 --push` and choose your registry tag.

To run container smoke checks locally:

```sh
docker buildx build --load --platform linux/amd64 \
  --build-arg VERSION=0.0.0 -t pulse:test-amd64 .
scripts/smoke-container.sh pulse:test-amd64 0.0.0 linux/amd64
```

Repeat for `linux/arm64`; execution on a different host architecture needs QEMU/binfmt support. The smoke checks verify inline configuration, explicit-file precedence, the supervisor, and the non-root image. A test-only `cloud-smoke` target runs native cloud credential/API tests in the same read-only distroless runtime.

## Release packaging

```sh
scripts/build-release.sh 0.0.0
scripts/package-release.sh 0.0.0
node scripts/smoke-npm.js 0.0.0
```

These commands build and validate local artifacts; they do not publish. The [release guide](docs/releases.md) describes GitHub release assets, npm publishing, multi-architecture images, container archives, signing, and required repository configuration. The npm packaging follows [c2j's release implementation](https://github.com/colony-2/c2j/tree/main/npm/c2j), with Pulse-specific installation and process-control tests.

## Making changes

Keep job lifecycle decisions in c2j/JobDB and provider-specific admission inside the adapters. Preserve the supplied execution environment and correlation metadata. Add regression coverage for behavior changes, run the relevant tests, and update configuration/protocol examples when their contracts change. Describe the resulting behavior and validation in your pull request.

Useful references:

- [Architecture and scheduling](DESIGN.md)
- [Implementation milestones](IMPLEMENTATION.md)
- [Remote protocol](REMOTE_PROVIDER_PROTOCOL.md) and [OpenAPI schema](api/provider.openapi.yaml)
- [Implementing a remote provider](docs/implementing-a-remote-provider.md)
- [Docker capacity design](LOCAL_DOCKER_PROVIDER.md)
- [c2j execution tracking guide](GUIDE-Execution-Tracking.md)
- [c2j public listing API contract](C2J_FEATURE_REQUESTS_RESPONSE.md)

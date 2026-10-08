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

Job discovery uses `github.com/colony-2/c2j/pkg/joblist`, pinned in `go.mod`. No c2j executable is needed to build or run the controller. Executor job images still need c2j. The supplied-lease integration pins c2j `v0.0.63-0.20261006024550-6709c8de92f6` and JobDB `v0.0.26`; there is no local module replacement.

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
go test -race -count=1 -timeout=15m -run '^TestDocker(C2J)?Integration$' -v ./internal/providers
```

Both tests use Pulse's core socket/context discovery. No socket override, opt-in flag, or local c2j installation is required; missing or unusable Docker fails the suite on Linux and macOS.

`TestDockerC2JIntegration` pulls `ghcr.io/colony-2/shai-mega:latest`, reports its digest and installed c2j version, and starts a real SQLite-backed JobDB HTTP server and Git HTTP repository. It invokes the native `pulse run --once` CLI with only `C2J_JOBDB`, so provider, image, platform, resources, and lease duration use production defaults. Pulse discovers and claims a submitted recipe, then hands the lease over stdin to the image's installed `c2j run with-lease`. The test checks job completion, saved output and execution artifacts, background heartbeat renewal, container-to-host access through `host.docker.internal`, persisted recipe failures, recipe timeout enforcement, and termination on initial or background renewal transport failures. The transport test drops an actual TCP connection and checks the reported `HTTP status 0` error. Workers receive no host mounts or replacement binaries. Heartbeat cases wait for protocol events, including the default lease's renewal interval, rather than asserting short wall-clock deadlines.

The HTTP fixture automatically selects a host interface reachable from the container. The same JobDB URI is used by native Pulse and container c2j. This models a reachable remote JobDB; it does not make a loopback-only JobDB service reachable from Docker. Go, Git (including `git http-backend`), the Docker CLI, permission to use the daemon, and registry access are required. The first image pull can be large. Each test cleans up its containers and database; downloaded images remain cached.

`TestDockerIntegration` separately checks process output, finite stdin and EOF, environment delivery, working directory, tmpfs and resource configuration, nonzero exits, restart accounting, capacity reuse, and active-instance listing using Alpine. Resource assertions check either enforced limits or the startup warning and recorded fallback mode. Unit tests cover both modes, socket/context selection, probe failures, cleanup, and recovery across capability changes. Live cloud IAM, networking, image access, resource enforcement, and workload execution still require deployment acceptance checks; report which checks you actually ran.

CI runs Go checks and real Docker jobs on Linux AMD64, Linux ARM64, and macOS Intel (using Colima for the CI daemon), cross-compiles all four release executables, tests npm installation, and smoke-tests both container architectures. See [the test workflow](.github/workflows/test.yaml).

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

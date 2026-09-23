# Contributing to Pulse

See the [README](README.md) for installation and use. This guide covers building, testing, and changing Pulse.

## Development setup

Use Go 1.26 or newer. Packaging checks also require Node.js 22+, Python 3, and standard Unix archive tools. Docker with Buildx is needed for container smoke checks.

```sh
make build          # bin/pulse and bin/pulse-exec
make test           # Go tests with the race detector
make vet
make test-packaging # npm installer tests
```

Job discovery uses `github.com/colony-2/c2j/pkg/joblist`, pinned in `go.mod`. No c2j executable is needed to build or run the controller. Executor job images still need c2j. The initial public API integration uses the published pseudo-version `v0.0.53-0.20260922032206-ef65f0001972`; there is no local module replacement.

When updating c2j, verify the public listing projection and execution compatibility against the new dependency. Keep the dependency pinned and run `go mod tidy -diff` to check module tidiness.

## Tests and validation

The Go suite covers scheduler concurrency/fallback, per-job cooldowns, provider state filtering and pagination, Docker accounting/recovery, cloud requests/authentication, HTTP diagnostics, and configuration precedence. The library adapter talks to a test server implementing the real JobDB HTTP protocol, exercising cancellation, tenant isolation, pagination, and execution demand projection.

CLI integration tests exercise file and inline configuration with an empty `PATH`, discover a job through JobDB, and submit it through the remote protocol. Continuous-mode tests verify public HTTP access and graceful shutdown.

Validate the remote OpenAPI contract and its examples in a Python environment:

```sh
python3 -m venv /tmp/pulse-protocol-venv
/tmp/pulse-protocol-venv/bin/pip install 'openapi-spec-validator==0.9.0' 'PyYAML==6.0.3'
/tmp/pulse-protocol-venv/bin/python scripts/validate_protocol.py
```

Cloud adapter tests use HTTP doubles for native APIs and credential endpoints. Docker adapter tests use a fake daemon API. Live cloud IAM, networking, image access, resource enforcement, and workload execution require deployment acceptance checks; report which checks you actually ran.

CI runs Go checks on Linux AMD64, Linux ARM64, and macOS, cross-compiles all four release executables, tests npm installation, and smoke-tests both container architectures. See [the test workflow](.github/workflows/test.yaml).

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

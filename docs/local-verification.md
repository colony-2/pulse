# Local build verification

`.c2/recipes/hooks/build/verify.yaml` implements `c2.build-hook/v1` for native
Linux arm64 workers. Each named const operation calls the project command
directly and independently declares its pinned tools. c2j supplies the candidate
checkout, installs tools, owns the process lifecycle and propagates operation
failures. There is no repository-local execution framework or Python driver.

| Task | Workflow/repository behavior | Native command |
| --- | --- | --- |
| `native_race` | test.yaml main-module race suite | `go test -mod=readonly -race -count=1 -timeout=45m -v -skip <exact-daemon-names> ./...` |
| `plugin_race` | test.yaml provider-plugin example | Same race flags in `examples/provider-plugin`, without the exclusion |
| `vet` | `make vet` | `go vet -mod=readonly ./...` |
| `native_build` | `make build` | Build `./cmd/pulse`, then `./cmd/pulse-exec` with `CGO_ENABLED=0 GOOS=linux`, into the operation workdir's `build` directory |
| `node_packaging` | test.yaml Node packaging units | `node --test scripts/npm.test.js` |
| `protocol` | test.yaml protocol validation | `uv run --no-project --python python3 --with openapi-spec-validator==0.9.0 --with PyYAML==6.0.3 python scripts/validate_protocol.py` |
| `module_tidiness` | release.yaml “Check module tidiness” | `go mod tidy -diff` in the main module |
| `report` | Hook evidence summary | Write `verification.md` after all mandatory commands succeed |

The exact anchored race-test exclusion is written in the recipe. It contains only
`TestDockerIntegration`, `TestDockerC2JIntegration`,
`TestDockerMultiPlatformIntegration`, `TestDockerDependencyCacheIntegration`, and
`TestDockerNixFetchCacheIntegration`. Ordinary provider packages, Docker mocks
and loopback fixture tests remain included. CI remains unchanged and unfiltered.
New incompatible tests require a reviewed boundary rather than automatic skips.

Operations run from c2j's const candidate checkout. The nested plugin check
changes its working directory explicitly; the request's target directory is
unused. Request, target path, documents and settings remain data. The legacy
include has no optional-artifact schema, accepting absent, null and value-artifact
settings. Candidate/base values enter through environment bindings. Scoped
credentials and runtime context stay under c2j's management.

The former Python helper, private candidate copies/caches, signal handlers,
subprocess runner, JSON result files and status/evidence validator are removed.
Go uses readonly module flags for tests/builds, bounded parallelism and the pinned
local toolchain; build binaries go outside the checkout in the operation workdir.
Go/uv use worker-managed caches. Python is declared only by protocol validation.

`capture_diagnostics: always` asks c2j to retain each check's native execution log,
including command, exit outcome and output. The commands print candidate/base,
native platform and tool versions into those logs. The hook exports seven native
log references plus `verification.md` (eight references). Required references are
resolved through the recipe engine; missing required evidence cannot return a
successful hook. No custom log hashes or second result protocol are maintained.

Each command uses the shell's fail-on-error behavior. A failed command stops its
operation and propagates through the hook to the existing caller, which reports
`failed` with c2j's failure diagnostics. Cancellation and timeout handling belong
to c2j. The sequence fails fast: later checks and the success report are not run
after failure. For the two build commands, the second is not run if the first
fails. The hook's `passed` output is reached only after every operation succeeds;
stdout text is never interpreted as a status. This replaces the former local
continue-and-aggregate machinery with the surrounding job's failure behavior.

The pinned nixpkgs revision is `39ad350a0602fa0a58a544344e3e9187526ea45c`.
Go 1.26.8 meets the module's Go 1.26 minimum; this does not claim exact release
Go 1.26.1 parity. Protocol dependencies retain the workflow's exact library pins.
The pinned Python 3.14 interpreter differs from workflow Python 3.13.

Held coverage remains in GitHub Actions: Docker daemon/image/cache integration,
QEMU/buildx and alternate architectures, macOS, live cloud/remote providers,
release cross-build/archive/npm packaging and smoke installation, signing,
tagging, publication, promotion and deployment. Optional infrastructure does not
enable these locally. Provider/installer/protocol mocks stay included.

Run focused native recipe regressions with:

```sh
c2j test run --file .c2/recipes/tests/verify.test.yaml --case-timeout 5m --artifact-mode inline --out-dir /tmp/pulse-hook-tests
```

These use tiny Go fixtures and the real Node/protocol checks. Five deliberately
failing daemon fixtures prove exact exclusions; ordinary provider/loopback
fixtures must execute. Negative cases verify that race, build and module-tidiness
failures reach the caller, retain native diagnostics, preserve the candidate and
prevent the success report. They replace tests of the removed helper framework;
existing application and CI tests are unchanged. Full source verification uses
the hook against the actual candidate, rather than these bounded Go fixtures.

# Instance provider plugins

Pulse can start a trusted local executable as a `type: plugin` compute provider.
Plugins use HashiCorp go-plugin over automatically authenticated local gRPC.
The public Go SDK is `github.com/colony-2/pulse/pkg/providerplugin`; compute types
remain in `pkg/compute`. Existing Docker, cloud, and remote providers are unchanged.

## Install and configure

Install an executable for the **controller's** OS and architecture at an absolute,
operator-controlled path. Linux and macOS are supported. Pulse does not search
PATH, invoke a shell, download plugins, or reload configuration while running.

```yaml
providers:
  custom:
    type: plugin
    plugin:
      command: /opt/pulse/plugins/custom-provider
      args: []
      pass_env: [CUSTOM_TOKEN]
      # sha256: <64 hexadecimal characters from sha256sum /opt/pulse/plugins/custom-provider>
      start_timeout: 30s
      shutdown_timeout: 5s
      max_concurrent: 4
      max_message_bytes: 16777216
      config:
        # This schema belongs to the installed provider.
        endpoint: https://backend.example.com
        token_env: CUSTOM_TOKEN

defaults:
  platform: linux/amd64

targets:
  - instance_id: production
    jobdb: https://jobdb.example.com/tenant
    jobdb_token_env: JOBDB_TOKEN
    launch_services:
      - name: custom
        priority: 1
```

`pulse check --config /etc/pulse/pulse.yaml` starts, configures, checks readiness,
and closes the provider without submitting work. It executes trusted plugin code
and can perform backend reads. `version` and `--help` are conventions for plugin
executables; the reference executable implements both.

An explicit configuration file takes precedence over inline `PULSE_CONFIG`, then
`pulse.yaml`. Relative provider file references resolve against the configuration
file's directory. Inline configuration uses the working directory captured when
Pulse loads it. Each subprocess runs in a different private temporary directory.
Omitted `plugin.config` is `{}`. It must otherwise be a single mapping of at most
1 MiB; aliases and duplicate keys are forbidden. Values are literal YAML.

Only variables named in `pass_env` and framework variables reach the process.
Present values are snapshotted at startup; absent variables are omitted. `HOME`,
proxy settings, and cloud credential variables require explicit selection. Names
beginning with `PLUGIN_` or `PULSE_PLUGIN_` are reserved. Provider environment is
never automatically added to worker environment. Do not forward Pulse's JobDB
claim/discovery credential. The worker's supplied lease is secret stdin data.

Pulse redacts the entire configuration block and argument values in `/config`.
Subprocess output is discarded. Initialization and native errors are replaced by
fixed safe categories. Authors must still keep secrets out of their own logs,
submission reasons, implementation identity, and instance metadata/references.
Plugin protobuf debug formatting is **not** secret-aware.

For a distroless deployment, copy a static executable into a derived Pulse image
or mount it read-only. Provide its runtime libraries, CA files and credential
files explicitly, and retain writable temporary storage for local sockets and
private process directories. No plugin executable is required by deployments
using built-in providers. Worker images/platforms and controller binaries can
have different architectures. Plugin workers receive the configured JobDB URL
without Docker's hostname translation.

## Implement a provider

Create a separate module, pin a Pulse release, and import only public packages.
The [reference module](../examples/provider-plugin) uses a local `replace` solely
for checkout development. It talks to a deterministic native HTTP fixture; it is
not a supported production compute backend.

```go
func main() {
    providerplugin.Serve(func(ctx context.Context, init providerplugin.Init) (compute.Provider, error) {
        var settings Settings
        if err := providerplugin.DecodeConfig(init.ConfigYAML, &settings); err != nil {
            return nil, err
        }
        // Validate required settings, read referenced files through ResolvePath,
        // initialize credentials, and reconstruct admission accounting.
        // ctx bounds initialization; init.Lifetime is for background loops.
        return newProvider(ctx, init.Lifetime, settings)
    }, providerplugin.ServeOptions{
        Implementation: "custom-provider",
        Version: "1.0.0",
        Shutdown: shutdownBackgroundLoops,
    })
}
```

`DecodeConfig` uses strict typed YAML decoding, rejects unknown fields and multiple
documents, and never returns raw parse errors. Validate required fields and
conflicting credential sources yourself. `ResolvePath(init.BaseDir, reference)`
resolves file names independently of subprocess working directory. Factories must
not submit workloads. A process can be configured only once, and business RPCs
are unavailable until initialization succeeds.

Implement `Submit(context.Context, []compute.Launch)` and
`List(context.Context, compute.ListRequest)`. Calls can overlap. Preserve all
allocation, process, correlation metadata, stdin, and deadline fields. Enforce
resources, runtime limits and start deadlines or decline before side effects.
Do not daemonize or leave helper processes behind. Runtime enforcement of accepted
work must remain independent of the plugin's lifetime.

A `Shutdown` hook stops provider background work and closes provider resources;
it must not terminate accepted native workloads. Cancellation likewise does not
mean rollback. The SDK watches a dedicated parent pipe to exit on abrupt host
loss. Hooks must honor their context and reap any helper children.

## Submission safety

A failed RPC may have launched work. After the host invokes Submit, transport
loss, deadline expiry, cancellation, and malformed/oversized responses yield
`unknown`. The scheduler retains the lease and does not fall back. Neither the
host nor the SDK replays submissions. Native adapters must follow the same rule.

Only `no_capacity`, `unsupported`, and `unavailable` allow fallback. They promise
that the declined submission cannot subsequently start. Safe application declines
are successful RPC responses with explicit per-item results, never gRPC errors.
Local validation failures produce `rejected`; unavailable processes and concurrency
overload produce `unavailable` before dispatch.

Providers can return results together with a Go error. The SDK preserves valid
results, supplies a safe diagnostic code and marks missing items `unknown`.
Duplicates, invalid statuses and malformed reasons poison only the affected
requested ID. Unsolicited IDs produce a diagnostic without erasing independent
acceptances. Reasons must be safe UTF-8, at most 4 KiB, and contain no NUL/newlines;
non-accepted statuses require a reason. Enum zero is invalid.

Listing is observational and must return only active managed instances. It must
preserve original metadata, exact launch filters and opaque pagination, including
empty pages with a continuation token. Missing instances never authorize replay.
List failure is an error, not a successful empty page.

## Limits, lifecycle, and compatibility

Protocol major 1 uses the Pulse cookie and `instance_provider` interface; JobDB
secret providers use a different protocol. Generated code is committed under
`pkg/providerplugin/proto/v1`; the source is
[`proto/providerplugin/v1/provider.proto`](../proto/providerplugin/v1/provider.proto).
The v1 baseline includes finite stdin and start deadlines, without optional flags.
New execution constraints need explicit feature negotiation or a major version.
SDK hosts may require optional features using `ProcessConfig.RequiredFeatures`.

Launch batches contain 1–100 items. Each stdin is at most 1 MiB, valid UTF-8 and
NUL-free. Page sizes are 1–100, cursors at most 16 KiB. Message limits default to
16 MiB, range from 1 MiB + 1 KiB to 128 MiB, and are fixed on each side at startup.
The smaller advertised limit applies. An oversized aggregate batch is rejected
before dispatch, never split. Size limits count serialized protobuf messages;
memory use is larger because domain and transport copies coexist. A local Linux ARM64 benchmark of four concurrent 15 MiB payload round trips
allocated about 315 MiB total (allocation volume, not peak resident memory).
Measure memory under actual concurrency before raising limits; `max_concurrent` defaults to 4
and accepts 1–1024. Admission has no queue, and lifecycle operations bypass it.

The returned `Process` owns its lifetime independently of the initialization
context. Always defer `Close()`. Startup respects both the caller deadline and
`start_timeout`. Business calls are capped by Pulse’s `call_timeout` (two minutes by default),
or `ProcessConfig.CallTimeout` for SDK hosts. Earlier caller deadlines apply.
A deadline on one business call does not kill unrelated calls.
Framework liveness checks are separate from backend readiness/capacity.

After an exit, the old process is reaped before replacement. At most three
replacement attempts use exponential delays beginning at one second with up to
25% jitter. Five healthy minutes reset the budget. Exhaustion leaves the provider
failed until Pulse restarts. Replacements repeat checksum, handshake and Configure
using the same arguments, selected environment, YAML and base directory snapshot.
Referenced credential files may change according to provider-specific semantics.
A restart never replays work or changes native scope.

`GET /providers/health` reports plugin name, implementation/version, protocol,
state, restart count, generation and a safe last failure category. It reports
process lifecycle, not backend capacity or job status. Existing `/status` poll
semantics and `/instances` error behavior remain intact.

The reviewed dependencies are go-plugin 1.8.0, protobuf 1.36.11 and gRPC 1.83.2;
the latter retains the version required by Pulse's existing cloud dependency
graph. gRPC application retries are disabled. Transparent retries are allowed
only where gRPC knows the server application has not processed the request.
Binary compatibility follows the protocol, not identical SDK/toolchain revisions.
Upgrade host and plugin independently after conformance testing.

## Test and regenerate

```sh
go test -race ./pkg/providerplugin/... ./internal/config ./internal/scheduler ./internal/httpapi
(cd examples/provider-plugin && go test -race ./...)
go test -race -run TestCLIPluginLifecycle ./cmd/pulse
make test vet build test-packaging
# scripts/smoke-container.sh also runs the provider-smoke distroless target.
```

`plugintest.Check` is reusable against a disposable native scope with an active
fixture launch. The SDK tests also exercise real subprocess crashes after native
start, no scheduler fallback or unused-lease release, cancellation, restarts,
configuration snapshots, overload, checksum failure and abrupt host loss.
Validate each intended production backend's credentials, native admission,
resource/deadline enforcement and execution independently before claiming support.

Regenerate using protoc 33.5 and the pinned Go generators:

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1
protoc -I proto --go_out=. --go_opt=module=github.com/colony-2/pulse \
  --go-grpc_out=. --go-grpc_opt=module=github.com/colony-2/pulse \
  proto/providerplugin/v1/provider.proto
```

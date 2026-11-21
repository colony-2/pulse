# Instance provider plugins

Use `type: plugin` to connect Pulse to a provider that implements
[`pulse-provider-api`](https://github.com/colony-2/pulse-provider-api).
Pulse can start a local provider executable, call a hosted HTTP endpoint, or
accept a provider-initiated connection. Each receives the structured launch
assignment with the requested base selector, capabilities, JobDB reference,
lease, and deadlines. The provider resolves the execution environment and
constructs the c2j invocation.

## Local executable

On Linux, build the executable with the standalone
[`pulse-provider-api/plugin` SDK](https://github.com/colony-2/pulse-provider-api/tree/2919f1ed5e42893f759af0fdd930c1a66824887b/plugin)
and install it at an absolute, operator-controlled path.

```yaml
providers:
  custom:
    type: plugin
    plugin:
      command: /opt/pulse/plugins/custom-provider
      args: []
      pass_env: [CUSTOM_TOKEN]
      start_timeout: 30s
      shutdown_timeout: 5s
      max_concurrent: 4
      max_message_bytes: 16777216
      config:
        # This schema belongs to the installed provider.
        endpoint: https://backend.example.com
        token_env: CUSTOM_TOKEN

targets:
  - instance_id: production
    jobdb: https://jobdb.example.com/tenant
    jobdb_token_env: JOBDB_TOKEN
    launch_services:
      - name: custom
        priority: 1
```

`pulse check --config /etc/pulse/pulse.yaml` initializes and closes local
plugins without submitting jobs. Pulse launches the command directly in a
private temporary directory. The SDK authenticates the local gRPC transport.
Use `sha256` to pin the executable's digest when needed. Distroless deployments
need the plugin executable and any runtime libraries, credential files, and
writable temporary storage it requires.

`plugin.config` is a YAML mapping with a 1 MiB limit and defaults to `{}`.
Resolve file references from `init.BaseDir`, which is the configuration file's
directory or the working directory for inline `PULSE_CONFIG`. The subprocess
only receives variables explicitly selected by `pass_env` plus framework
variables. The shared SDK reserves `PLUGIN_*`, `PULSE_PLUGIN_*`, and `TMPDIR`.
Pulse sends `defaults.env` separately in `init.ExecutorEnv`. The factory must apply
those values to executors or reject initialization. Keep JobDB claim/discovery
credentials on the controller.

A Go plugin calls `plugin.Serve` with a factory of type
`func(context.Context, plugin.Init) (provider.Provider, plugin.Cleanup, error)`.
Implement `GetCapabilities`, `Submit`, and `List` as described in the
[Go provider guide](implementing-a-go-provider.md). The factory context bounds
startup. Use `init.Lifetime` for maintenance and detach accepted native work
from the process and RPC lifetimes. The
[reference executable](../examples/provider-plugin) is a separate Go module that
forwards structured assignments to a test fixture.

The shared SDK uses protocol 3, the default on Linux. Rebuild protocol 2 providers
with the updated SDK. Executables built with Pulse's `pkg/providerplugin` use
protocol 1. Set `plugin.protocol: 1` to
keep running an existing binary on Linux during migration. macOS continues to
default to protocol 1 because the shared process SDK supports Linux. The v1
[protobuf contract](../proto/providerplugin/v1/provider.proto) carries concrete
image, platform, and process fields.

Pulse uses `call_timeout` to bound business calls. On failure, it can make up to
three replacement attempts, with exponential delay and jitter. After five
healthy minutes the restart budget resets. Restarting does not replay
submissions. `GET /providers/health` shows local plugin lifecycle and protocol.
Pulse redacts executable arguments and plugin configuration in `GET /config`.

## Reverse connection

Run Pulse continuously with a reverse collector listener. Each `type: reverse`
provider uses its configured name as its SDK identity and reads its shared key
from the environment variable named by `token_env`. Add the provider name to
the target's `launch_services` to schedule work there. One collector can accept
several configured identities with separate keys.

```yaml
reverse:
  listen: "0.0.0.0:8443"
  tls_cert: /etc/pulse/collector.pem
  tls_key: /etc/pulse/collector.key
providers:
  mac-worker:
    type: reverse
    token_env: MAC_WORKER_KEY
targets:
  - jobdb: https://jobdb.example.com/tenant
    launch_services: [{name: mac-worker, priority: 1}]
```

TLS is the default. The provider verifies the collector certificate and uses
the shared key for admission. Set `reverse.transport: http` and omit the TLS
files for plaintext HTTP/2 on an isolated test network. The shared key and
provider traffic travel unencrypted in this mode. Both ends need protocol 3
and the same transport. Configure executor environment variables on the remote
provider.

For example, start a plaintext test worker with the same key available in its
`MAC_WORKER_KEY` environment variable.

```sh
pulse-macvz server --collector 127.0.0.1:8443 --name mac-worker \
  --reverse-auth mutual-key --reverse-transport http \
  --shared-key-env MAC_WORKER_KEY
```

See the [pulse-macvz command guide](https://github.com/colony-2/pulse-macvz/blob/9d3589b5bb37b84b2a096a2f95f5fe39813bde9b/docs/server-cli.md)
for the TLS variant and the [SDK guide](https://github.com/colony-2/pulse-provider-api/tree/2919f1ed5e42893f759af0fdd930c1a66824887b/plugin#outbound-providers-and-the-central-collector)
for using `plugin.Run` in another provider.

## HTTP endpoint

Configure the service URL and the environment variable holding its bearer token.
Use the existing target `launch_services` setting to select the provider.

```yaml
providers:
  hosted:
    type: plugin
    plugin:
      endpoint: https://providers.example.com/compute
      token_env: COMPUTE_PROVIDER_TOKEN
```

HTTPS is required unless you explicitly set `allow_http: true`. The endpoint may
include a path prefix and must omit credentials, query strings, and fragments.
Pulse sends the bearer token on each request, bounds requests by `call_timeout`,
limits responses to 16 MiB, and declines redirects. Configure the executor
environment at the service and leave Pulse's `defaults.env` empty.
Process settings such as `command`, `pass_env`, and `config` belong to local
executables.

The HTTP service uses these JSON operations relative to the configured URL.

| Method and path | Request and successful response |
| --- | --- |
| `GET /v1/capabilities` | Returns `provider.Capabilities`, including the ordered `bases` and `bases_complete`. |
| `POST /v1/submit` | Accepts `{"items":[...]}` with `provider.Launch` objects. Returns `{"results":[...]}` with `provider.Submission` objects. |
| `GET /v1/launches` | Accepts `page_size`, optional `page_token`, and optional exact `launch_id` as query parameters. Returns `provider.ListResponse`. Use `"items":[]` for an empty page. |

For example, a submission response can include definite per-launch results and a
method error together.

```json
{
  "results": [
    {"launch_id": "launch-1", "status": "accepted"},
    {"launch_id": "launch-2", "status": "no_capacity", "reason": "all workers busy"}
  ],
  "error": "inventory refresh failed after admission"
}
```

Use HTTP 200 for responses with authoritative per-launch results, including
definite declines and results returned alongside an optional `error` string.
After dispatch, an HTTP failure, timeout, or missing or invalid result leaves
the affected launch uncertain. Pulse retains its lease and never automatically
replays the submission or falls back on uncertainty. Only explicit
`no_capacity`, `unsupported`, or `unavailable` decisions allow fallback.
Exclude credentials and lease data from reasons, errors, and instance listings.

Existing `type: remote` services use the
[process-level remote protocol](../REMOTE_PROVIDER_PROTOCOL.md). HTTP plugin
services use `job`, `base`, and `lease` on each structured assignment, and
return typed `job` references in listings.

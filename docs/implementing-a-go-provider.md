# Implementing a Go provider

Pulse schedules through the dependency-free
[`pulse-provider-api`](https://github.com/colony-2/pulse-provider-api) contract.
An implementation in an independent Go module needs only that module:

```go
import provider "github.com/colony-2/pulse-provider-api"

var _ provider.Provider = (*Provider)(nil)

// Implement:
// Submit(context.Context, []provider.Launch) ([]provider.Submission, error)
// List(context.Context, provider.ListRequest) (provider.ListResponse, error)
```

A launch includes allocation requirements (`Base`, `Capabilities`, CPU, memory,
scratch), a typed `JobRef`, an opaque exported `Lease`, and optional deadlines.
The provider owns executor startup, supplies the exact lease to c2j, and reports
verified execution allocation facts. It must not claim replacement work. Keep
credentials out of diagnostics, argv, the child environment, and instance lists.
The current c2j integration still reports concrete image/platform allocation
facts; Pulse's native adapters also expose the resolved label as
`PULSE_EXECUTION_BASE`. A selector is never an allocation fact.

## Link an implementation into a custom Pulse host

Keep registration in your host module so the provider implementation remains
independent of Pulse core. Use `pkg/providerregistry.Register` before creating
`pkg/pulsecli.NewCommand`:

```go
package main

import (
    "context"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    provider "github.com/colony-2/pulse-provider-api"
    "github.com/colony-2/pulse/pkg/providerregistry"
    "github.com/colony-2/pulse/pkg/pulsecli"
    acme "example.com/acme/pulse-provider"
)

func main() {
    err := providerregistry.Register("acme", func(ctx context.Context, opts providerregistry.Options) (provider.Provider, func(), error) {
        // Decode opts.Config (JSON) into your own configuration type.
        // opts.BaseDir resolves relative paths; opts.Env contains executor defaults.
        return acme.New(ctx, opts.Config)
    })
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()
    if err := pulsecli.NewCommand("custom").ExecuteContext(ctx); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

The example assumes your `acme.New` returns `(provider.Provider, func(), error)`;
adapt the constructor call to your implementation. Factories receive a bounded
initialization context, not a workload-lifetime context. Pulse invokes returned
cleanup on shutdown or failed initialization. Return cleanup alongside an error
if partial construction needs cleanup. Provider calls can overlap.

```yaml
defaults:
  base: 'ubuntu-26*arm64'
providers:
  workers:
    type: acme
    config:
      endpoint: https://runners.example
      token_env: ACME_TOKEN
targets:
  - jobdb: https://jobdb.example/acme
    launch_services: [{name: workers, priority: 1}]
```

Unknown/unregistered types fail configuration validation. Registration cannot
replace built-ins or another registration. Provider `config` values and executor
environment values are redacted from the public configuration endpoint. External
providers receive the original selector and capabilities; Pulse does not limit
them to its native container catalog.

## Base selectors

Match case-sensitively against concrete names and explicitly declared family
aliases. Each `*`, anywhere, matches zero or more characters, including `/`.
Repeated stars are allowed. All other characters are literal: there are no `?`
wildcards, character classes, escapes, regexes, or version comparisons. Match
the entire name or alias. Empty selectors are invalid.

For example, `ubuntu-26.04-arm64` with aliases `ubuntu` and `linux` matches
`ubuntu-26*arm64`, `ubuntu*`, `linux*`, and `*`. There is no implicit ordering or
preference for the newest match. Select only candidates satisfying every resource,
capability, and deadline requirement. Providers must agree on label meanings for
portable fallback; an alias describes the executor environment, not its host.

## Existing engines and transports

Docker, Cloud Run, ECS, Azure Jobs, and the v1 remote/plugin clients are connected
through the structured API using native adapters. Those adapters resolve bases,
construct c2j commands, deliver leases, and translate native metadata into typed
job references. They reject nonempty capability requirements because these
adapters do not currently provision or verify optional interfaces such as Docker
or KVM inside the executor. Installing a CLI alone does not establish capability.

`pkg/compute` and `pkg/providerplugin` retain the existing process-level v1 wire
contract for compatibility. Existing remote services and plugin binaries can
continue to receive concrete image/platform/process requests. They do not receive
unresolved selectors or new capability claims. Go providers registered directly
use the standalone API and need neither transport nor the legacy compute types.

Native resources retain the six previous correlation labels plus `pulse_jobdb`
(the credential-free executor endpoint). For pre-migration resources missing
that label, configured instance/tenant targets supply the endpoint. Legacy
resources outside those targets cannot produce a complete `JobRef` and listing
reports an error instead of inventing an endpoint. Instances never include leases.

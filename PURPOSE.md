# Pulse

Pulse provisions compute for c2j jobs across all repositories in configured
JobDB tenants. It connects work awaiting an executor to local Docker, remote
runner services, Google Cloud Run Jobs, AWS ECS Fargate, and Azure Container Apps
Jobs.

## Responsibilities

- Discover eligible work through c2j's public Go library and acquire a JobDB
  lease before submitting compute. Carry job identity, execution requirements,
  and the supplied lease into the executor.
- Select providers by configured priority, rotate equal-priority services, and
  handle partial acceptance and capacity fallback. Bound concurrent claims and
  suppress repeated provisioning failures with cooldowns.
- Translate resource and process requirements into provider launches, track
  active compute through provider inspection, and manage provider-specific
  capacity and lifecycle behavior. The Docker adapter also manages tenant-scoped
  dependency cache volumes.
- Supply configuration, cloud authentication integration, diagnostic logging,
  and read-only controller inspection for operators.

The implementation and its tests live in [internal/controller](internal/controller),
[internal/c2j](internal/c2j), [internal/scheduler](internal/scheduler), and
[internal/providers](internal/providers). Direct library and SDK dependencies are
declared in [go.mod](go.mod).

## Interfaces

- [cmd/pulse](cmd/pulse) provides the controller CLI: continuous polling,
  a single discovery/submission pass, configuration checks, and version output.
  YAML configuration and tenant selection are described in
  [docs/configuration.md](docs/configuration.md).
- [pkg/compute](pkg/compute) defines provider-neutral launch, submission, and
  instance-listing contracts. Remote runner services implement the HTTP contract
  in [api/provider.openapi.yaml](api/provider.openapi.yaml); see
  [docs/implementing-a-remote-provider.md](docs/implementing-a-remote-provider.md).
- [cmd/pulse-exec](cmd/pulse-exec) and
  [internal/supervisor](internal/supervisor) bridge process input for cloud
  launches and supervise process groups, including explicitly configured
  infrastructure time limits and start deadlines.
- [internal/httpapi](internal/httpapi) exposes the continuous controller's public,
  unauthenticated, read-only status, redacted configuration, active instances,
  cooldowns, and scheduling rotation. See [docs/http-api.md](docs/http-api.md).
- Native releases, the controller container, and the
  [npm/pulse](npm/pulse) installer distribute the executables. Usage and provider
  operations are documented in [README.md](README.md) and
  [docs/providers.md](docs/providers.md).

## Ownership boundaries

Pulse owns discovery, lease acquisition, placement, and compute provisioning.
It does not implement recipe execution. The controller uses c2j as a library
and needs no separate c2j executable; executor images must provide c2j with
supplied-lease support and the tools their recipes need. Cloud executor images
also need the execution bridge.

c2j owns recipe and operation timeouts and renews the supplied lease during
execution. Pulse does not renew leases or impose a default execution timeout;
optional infrastructure caps depend on provider support. Definite non-starts
release the lease with backoff, while accepted or uncertain starts rely on lease
expiry for recovery.

JobDB owns durable jobs, execution state, and lease arbitration. Pulse uses a
remote HTTP(S) tenant rather than serving a database or maintaining a separate
durable work queue. Controller cooldowns are in memory; durable ownership stays
in JobDB. Broad JobDB discovery credentials remain with the controller rather
than being passed to executors.

Provider adapters and external runner services own their compute mechanisms;
the provider contract carries executable process requests rather than requiring
remote services to interpret c2j recipes or JobDB semantics. Resource enforcement
and cleanup capabilities vary by provider. Historical proposals and feature
requests in this repository describe design context, not additional implemented
responsibilities.

# Local Docker provider

Status: design for the built-in `docker` adapter. See [README.md](README.md) for current implementation and operational limitations. It uses the same batch preparation/submission contract and participates in numeric priority tiers.

## Capacity policy

Docker containers have no resource constraints by default; CPU and memory limits bound individual containers. Our admission policy must also bound the sum of their allocations. Use a configured pool budget rather than admitting work based on low instantaneous utilization. See [Docker resource constraints](https://docs.docker.com/engine/containers/resource_constraints/).

Initial scope: a local Linux Docker Engine with working CPU, memory, and swap-limit enforcement. One Cortex process owns admission to that daemon. Configure an explicit budget after leaving headroom for the host, Docker, image operations, and other workloads:

```yaml
providers:
  local:
    type: docker
    socket: unix:///var/run/docker.sock
    capacity:
      cpu: "8"
      memory: 16Gi
      max_containers: 4
    per_container_memory_overhead: 256Mi
    scratch:
      type: tmpfs
      path: /scratch
```

This provider instance can be referenced by a target as `{name: local, priority: 1}`. The example budget is illustrative, not an automatically detected machine size. Startup validates it against the daemon's available CPU/memory and required enforcement features. The budget is an operator-reserved pool; unrelated processes or unrestricted containers cannot be assumed to respect it. Use a dedicated daemon/workload host or explicitly reserve and enforce the budget outside that pool.

For an allocation with CPU `C`, usable memory `M`, scratch `S`, and configured per-container overhead `H`, charge:

```text
CPU charge    = C
memory charge = M + S + H   # initial tmpfs scratch backend
slot charge   = 1
```

An item fits only if every committed sum plus its charge stays within the corresponding budget. Never use current CPU load, RSS, or currently written scratch bytes to reduce its charge. For example, in the sample pool, four 2-CPU/2-GiB-memory/1-GiB-scratch launches consume 8 CPUs, 13 GiB including overhead, and all four slots. Further items return `no_capacity` even while those containers are idle.

## Batch admission

`Prepare` resolves the image/platform and rounds resource settings as needed, then simulates admission in input order against current committed capacity. It can return plans for a subset and `no_capacity` for the rest. A request too large for the total configured pool is `unsupported`. A busy pool is `no_capacity`. Neither result queues a container.

Preparation does not reserve capacity. `Submit` refreshes Docker state and atomically reserves charges for a fitting subset under a daemon-wide in-process mutex. It then performs bounded image/create/start work, keeping reservations until those charges are represented by Docker containers or the attempt definitively fails. Concurrent batches share this accounting. Do not count a reservation and its created container twice.

Review each item in batch order and continue after a non-fitting item; a later smaller item may fit. Return exactly one result per item. Capacity that disappears between preparation and submission results in `no_capacity`, allowing Cortex to send that item to another service. There is no local pending-work queue in version 1.

The adapter rechecks and honors the prepared configuration before starting. Docker limit enforcement failure is not permission to start unconstrained. A confirmed failed submission releases its in-memory reservation only after no delayed start remains possible. An uncertain create/start response retains its charge until reconciliation establishes the outcome and returns `unknown` to Cortex.

## Recover accounting from Docker

The durable resource records are Docker's container configurations and labels, not a new Cortex database. Create each container with a deterministic name derived from the launch ID and labels containing the complete correlation envelope, provider identity, accounting version, resource charges, and a process/plan fingerprint. Docker supports creating a stopped container before starting it; see [container creation](https://docs.docker.com/reference/cli/docker/container/create/).

Before accepting work at startup, and before each admission batch, list and inspect managed containers. Count `created`, running, paused, restarting, and any uncertain/nonterminal containers against the pool. Count daemon-visible containers regardless of which Cortex process originally created them. A Docker read failure blocks new admission with `unavailable`; an empty local cache does not mean an empty host. Missing or inconsistent accounting for a managed container also blocks admission until reconciled.

In-memory reservations bridge admission to container creation. After a Cortex crash, containers already created carry their charges; work that never reached creation cannot start on its own. A delayed create may leave a stopped container: never automatically start it on recovery. Every start must pass accounting against current Docker state. A start already issued before the crash is covered by its previously created, charged container.

Terminal containers release CPU, memory, and slots once their stopped state is confirmed. Keep terminal records for the inspection/idempotency retention period, with automatic removal disabled. Disable Docker restart policies so a terminal container cannot consume released capacity later. Repeated submissions of a retained launch ID inspect and reuse its result; they do not restart it. Clean up abandoned never-started containers only after pending operations are resolved. Persisted labels are provider resource metadata, consistent with cloud provider records.

Require one admission owner per daemon, enforced by a process-lifetime OS lock keyed by daemon identity. Multiple configured aliases for the same daemon share one budget/accounting object. Separate processes must use the same host lock namespace; distributed access to a daemon belongs behind a single remote provider service instead. The lock stores no scheduling state. Other clients must not start, restart, or modify managed containers behind the adapter's accounting.

## Docker limits and scratch

Apply a CPU quota matching `C` and a hard memory limit of `M + S`; set the combined memory-plus-swap limit to the same value. Charge `H` as additional host headroom rather than advertising it as usable application memory. CPU quotas are ceilings, not dedicated cores; the pool policy prevents this adapter from oversubscribing its configured CPU budget. [Docker documents these limit semantics](https://docs.docker.com/engine/containers/resource_constraints/).

Version 1 supplies an explicitly sized Linux tmpfs at the configured scratch path. Tmpfs usage counts against the container's memory limit, so reserve its full size alongside application memory. Advertise `M` as usable execution memory and `S` as scratch, never `M + S` as execution memory while also promising `S` scratch. The image/runtime must use the declared scratch path; ordinary writable-layer capacity is not a scratch guarantee. See [Docker tmpfs mounts](https://docs.docker.com/engine/storage/tmpfs/).

Bound logs and reserve operational disk headroom for images, container layers, and retained records. Host disk monitoring remains an operational responsibility in the initial implementation; a future advisory availability check would not be a disk-space reservation. Disk quotas and guaranteed disk-backed scratch are not supported in the current Docker provider scope. Scratch is limited to the tmpfs mode above; an ordinary bind mount or free-space check is not a capacity guarantee. Requests this mode cannot satisfy fall through to another provider.

## Lifetime and allocation facts

Resolve images for the host's native platform, bind the launch to the prepared content, and keep reference/manifest/config identities distinct. Unsupported platforms return `unsupported` rather than silently using emulation. Preserve c2j's required image-reference matching when pinning content.

A provider-supplied in-container supervisor enforces the execution timeout and any start deadline without depending on Cortex remaining alive. It launches the supplied process, forwards signals, and terminates the workload at the limit. The implementation must provide this helper for supported images/platforms before claiming timeout support; a Cortex-only timer is insufficient. The container's original application image remains the requested image.

## Implementation checks

- Fit part of a batch and decline the rest; exercise fallback through priority tiers.
- Concurrent batches cannot reserve more than the CPU, memory, or slot budget.
- Idle usage does not free committed capacity; tmpfs is charged in addition to usable memory.
- Cortex restart reconstructs charges for running and never-started containers before new admission.
- Create/start timeouts retain uncertain charges; stopped-state confirmation releases them.
- Retried IDs do not restart completed containers; correlation survives failed startup and process restart.
- Limits, scratch configuration, and deadlines remain effective while Cortex is down.

This is a provider implementation plan; no Docker workloads are launched by this documentation change.

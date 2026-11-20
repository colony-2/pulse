# VZ provider

Pulse uses Apple Virtualization.framework through the linked
[`pulse-tart` Go library](https://github.com/colony-2/pulse-tart). The library
manages VM creation, guest bootstrap, lease delivery, capacity, recovery, and
cleanup.

## Automatic VZ configuration

When `providers` is omitted or empty, Pulse enables Docker and adds a provider
named `vz` on Apple Silicon with macOS 14 or later. Targets without
`launch_services` use Docker at priority 1 and VZ at priority 2. An explicit
provider map controls which providers are enabled.

Pulse leaves VM counts, bases, slots, resource sizes, and Linux Docker setup to
the library. The VZ provider defaults to **two macOS job VMs and four Linux job VMs**, each with
**4 vCPU / 8 GiB**. Linux includes Docker; macOS Docker remains opt-in.
Counts do not scale to host resources: six job VMs reserve 24 vCPU / 48 GiB
before host overhead. Use optional count overrides for smaller hosts.

Automatic detection preserves the default `ubuntu-26*` selector. Select a
macOS base with `defaults.base: 'macos*'`; Linux work continues to use Docker.
Use an explicit VZ-only configuration to run on a Mac without Docker. See [vz.yaml](../examples/vz.yaml).

```yaml
defaults:
  base: 'macos*'
providers:
  mac:
    type: vz
targets:
  - jobdb: https://jobdb.example/acme
    launch_services: [{name: mac, priority: 1}]
```

The library's default catalog is `macos-26-arm64`, `macos-15-arm64`, and
`ubuntu-24.04-arm64`, in that preference order. Every configured base gets a
conventional pool when slots are omitted. The two macOS versions share the same
two-job limit: either version can use both positions, or each can run one job.
The default eight configured positions (two per macOS version and four Linux)
remain bounded by six total job VMs.

To override only the job counts, leave the catalog and slots omitted:

```yaml
config:
  vm_counts: {macos: 1, linux: 2, total: 2}
```

Omitted per-OS counts use library defaults; an omitted total is their sum.
Zero disables an OS, or drains all job capacity when used for `total`. The
shared total caps jobs across both OSes. Counts include warming/idle job VMs
and unresolved reservations, but exclude Docker companions and image builders.
The library separately enforces the physical two-running-macOS-VM limit.

Image digests, OS versions, bootstrap credentials, and default capabilities come
from the library. Pulse does not maintain a second catalog. The library verifies
the guest tools before admission. Custom images use `ssh_password_env`; only
library-published images may use their published bootstrap login.
Each macOS job gets a fresh clone of a prepared, suspended slot. A busy or warming pool
returns `no_capacity`; leases are not queued while templates prepare.

## Configuration

The provider accepts a strict `config` mapping.

| Field | Meaning |
| --- | --- |
| `owner_id` | Stable ownership identity derived from the provider name by default. |
| `bootstrap_timeout` | Preparation deadline; default `45m`. |
| `vm_counts` | Optional `macos`, `linux`, and `total` job limits; omitted values use library conventions. |
| `bases` | Optional replacement catalog; omitted uses the library’s default macOS and Linux bases. |
| `slots` | Optional explicit groups; omitted creates a pool for every configured base, sharing the per-OS and total VM limits. |
| `docker` | Optional macOS Docker opt-in or advanced engine settings; Linux Docker is automatic. |
| `vz` | Optional VM storage, runner, timeout, and Docker endpoint settings. |

Each base accepts `name`, `os` (`darwin` by default or `linux`), digest-pinned `image`, `os_version`,
`ssh_username` (default `admin`), `ssh_password_env`, optional
`ssh_host_key_file`, and `capabilities`. The library verifies the actual guest OS
version and the capabilities declared in its catalog. Define slots with
`name`, `count`, concrete `base`, `cpu`, `memory_mib`, and optional
`docker_profile`. Explicit slots must reference bases in the configured catalog.
Explicit empty catalogs/groups are invalid; use zero VM counts to drain. Job and
engine VM sizes are currently fixed at 4 vCPU / 8192 MiB; other sizes are rejected.

| Field under `config.vz` | Meaning |
| --- | --- |
| `home` | VM inventory and cache directory. Defaults to `~/.pulse-vz`. |
| `runner` | Path to an optional standalone signed VZ runner. Defaults to the Pulse executable. |
| `command_timeout` | Native command deadline. Defaults to `2m`. |
| `ssh_timeout` | Guest SSH deadline. Defaults to `15s`. |
| `delivery_timeout` | Pre-execution delivery deadline. Defaults to `2m`. |
| `docker_endpoint` | Host IPv4 address and reserved port range for macOS Docker. |

Relative paths resolve against the configuration file directory, or the
working directory for inline configuration. Pulse reads credentials during
construction and redacts `config` from public diagnostics.

The embedded runner needs the `com.apple.security.virtualization` entitlement.
Pulse releases and `make build` on Apple Silicon include it. If you build
Pulse directly with Go, sign it before running VZ.

```sh
codesign --force --sign - --entitlements cmd/pulse/vz.entitlements ./pulse
```

Keep the provider name or explicit `owner_id` and VZ home stable across
restarts. Running VMs survive Pulse shutdown. The next runtime uses persisted
VM properties to recover ownership and execution state.

## Runtime behavior and limits

`pulse check` validates library configuration and checks the signed VZ runner and local inventory
without preparing or launching VMs. Once the controller runs, Pulse reconciles
slots in the background, retrying observation errors. Preparation has its own
deadline so a cold image download can outlast `call_timeout`. Shutdown cancels
reconciliation before closing the runtime. `run --once` does not wait for cold
slots to become ready; continuous operation prepares and maintains the pool.

Pulse uses the provider API's `GetCapabilities` for startup reporting and
scheduler filtering. It forwards the original selector, capabilities, and job
assignment to the library, which selects a concrete base and a fitting slot.
The host keeps no separate matching catalog. Discovery describes configured
support independently of free capacity; advertising a base does not provision
slots for it. Unknown or malformed submission responses never trigger another
launch attempt for that assignment.

Default guests are macOS ARM64 and Ubuntu 24.04 ARM64; Linux includes Docker.
Scratch reservations, positive infrastructure timeouts, start-before deadlines, and `defaults.env` are not
supported and are declined. Use c2j recipe timeouts. Capabilities and resource
requirements are passed to the library for admission and verification. The
legacy c2j image/platform compatibility path remains container-oriented: use
`defaults.base` for VZ and omit legacy image/platform requests.
Supply a JobDB URL reachable from the VM; Docker's loopback-host rewrite does not
apply to VZ.

The VZ runtime reports a complete configured catalog and passes the selected
concrete base to c2j through `--execution-base`.
The library resolves c2j during image preparation. Existing prepared images are
reused; upgrading Pulse does not reinstall c2j in those images.

## Docker in VZ guests

Linux Docker needs no profiles, slot definitions, or host endpoint. The
library provisions the integrated engine in the Linux job VM and reports its
`docker` capability through `GetCapabilities`.

For macOS Docker, set `docker: {}` and configure `vz.docker_endpoint` with
the host's guest-reachable IPv4 address and a port range reserved for this
provider. The library validates the endpoint and adds its default engine
profile to the conventional macOS groups. See [vz-docker.yaml](../examples/vz-docker.yaml).

Explicit slot configurations remain an advanced override: assign a
`docker_profile` to each intended group and configure the corresponding engine
profile. Discovery reflects those choices rather than adding host-side claims.

`docker.ready_timeout` accepts a duration. `docker.profiles` replaces the default
profile list. Each profile accepts `name`, optional digest-pinned `image`
(defaults to the library's engine image), `ssh_username`, `ssh_password_env`,
`ssh_host_key_file`, `cpu`, `memory_mib`, `disk_gib`, and `engine_version`.
Omitted resource/version values use library defaults. Do not add `docker` to a
base's manual `capabilities` list: the provider derives it from slot profiles
and verifies the engine before admission.

One macOS Docker allocation uses two VMs; one Linux Docker allocation uses one.
The current default companion reserves another 4 vCPU and 8 GiB, in addition to
the primary slot. Size the pool for this total. The primary advertises 512 MiB
less memory than the VM reservation for worker overhead.

Build contexts, copies, engine-owned volumes, and supported TCP services work
without host bind mounts. Job workspace bind mounts are unsupported. See the
[pulse-tart Docker contract](https://github.com/colony-2/pulse-tart/blob/0307afcf2be202b72c5bf4a8e281adefc50f1a76/docs/docker-library.md)
for the tested Docker/Compose behavior and VM lifecycle.

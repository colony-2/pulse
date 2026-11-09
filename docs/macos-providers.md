# Tart and Orchard providers

Pulse links the published [`pulse-tart`](https://github.com/colony-2/pulse-tart)
Go library for both `type: tart` and `type: orchard`. The library implements the
shared provider API and owns VM creation, guest bootstrap, lease delivery,
suspended templates, capacity accounting, recovery, and cleanup. No provider
plugin or copied implementation is involved.

## Automatic Tart configuration

When `providers` is omitted or empty, Pulse enables Docker and, on Apple Silicon
macOS with `tart` on `PATH`, also enables a provider named `tart`. Targets without
`launch_services` use Docker at priority 1 and Tart at priority 2. Explicit
provider maps and service lists remain authoritative. Intel macOS and Linux do not automatically
enable local Tart. Orchard requires explicit connection settings.

Pulse leaves VM counts, bases, slots, resource sizes, and Linux Docker setup to
pulse-tart's conventions. No `config` block is needed for local Tart. This
release defaults to **two macOS job VMs and four Linux job VMs**, each with
**4 vCPU / 8 GiB**. Linux includes Docker; macOS Docker remains opt-in.
Counts do not scale to host resources: six job VMs reserve 24 vCPU / 48 GiB
before worker overhead. Use optional count overrides for smaller hosts.

Automatic detection preserves the default `ubuntu-26*` selector. Select a
macOS base with `defaults.base: 'macos*'`; Linux work continues to use Docker.
To run only Tart, including on a Mac without Docker installed, configure it
explicitly as in [tart.yaml](../examples/tart.yaml). A minimal configuration is:

```yaml
defaults:
  base: 'macos*'
providers:
  mac:
    type: tart
targets:
  - jobdb: https://jobdb.example/acme
    launch_services: [{name: mac, priority: 1}]
```

The library's default catalog is `macos-26-arm64`, `macos-15-arm64`, and
`ubuntu-24.04-arm64`, in that preference order. Every configured base gets a
conventional pool when slots are omitted. The two macOS versions share the same
two-job limit: either version can use both positions, or each can run one job.
The default eight configured positions (two per macOS version and four Linux)
remain bounded by six total job VMs. There are no aliases; old explicit Sequoia
configurations must use the library's current label and image settings.

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

Both provider types accept a strict `config` mapping:

| Field | Meaning |
| --- | --- |
| `owner_id` | Stable library ownership identity; defaults to a hash of provider type/name. |
| `state_dir` | Durable private journal directory; defaults under the OS user configuration directory at `pulse/providers/<identity>`. |
| `bootstrap_timeout` | Preparation deadline; default `45m`. |
| `vm_counts` | Optional `macos`, `linux`, and `total` job limits; omitted values use library conventions. |
| `bases` | Optional replacement catalog; omitted uses the library’s default macOS and Linux bases. |
| `slots` | Optional explicit groups; omitted creates a pool for every configured base, sharing the per-OS and total VM limits. |
| `docker` | Optional macOS Docker opt-in or advanced engine settings; Linux Docker is automatic. |
| `tart` | Local settings for `type: tart`. |
| `orchard` | Required connection settings for `type: orchard`. |

Each base accepts `name`, `os` (`darwin` by default or `linux`), digest-pinned `image`, `os_version`,
`ssh_username` (default `admin`), `ssh_password_env`, optional
`ssh_host_key_file`, and `capabilities`. The library verifies the actual guest OS
version and the capabilities declared in its catalog. Define slots with
`name`, `count`, concrete `base`, `cpu`, `memory_mib`, and optional
`docker_profile`. Explicit slots must reference bases in the configured catalog.
Explicit empty catalogs/groups are invalid; use zero VM counts to drain. Job and
engine VM sizes are currently fixed at 4 vCPU / 8192 MiB; other sizes are rejected.

Local `tart` settings are `binary` (default `tart` on `PATH`), `home` (default
`~/.tart`), `command_timeout`, `ssh_timeout`, `docker_endpoint`, and optional
`python_binary` for the Docker endpoint supervisor. Orchard settings are `url`,
`worker`, optional `worker_labels`, `username`, `token_env`, `ca_cert_file`,
`request_timeout`, `docker_endpoint`, and `allow_http` for explicit lab HTTP connections. See
[orchard.yaml](../examples/orchard.yaml). Credentials are read at construction;
`config` is redacted from the public diagnostics endpoint. Relative file paths
resolve against the Pulse configuration file directory (working directory for
inline configuration). A bare Tart binary name is resolved on `PATH`.

Keep owner/state identity stable across restarts. One runtime owns each journal;
separate providers need separate ownership scopes and journals. Do not remove
state to retry an uncertain launch. Closing Pulse releases the library's local
resources while native VMs survive; the next process recovers the same journal.

## Runtime behavior and limits

`pulse check` validates library configuration and checks backend connectivity
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
`defaults.base` for these macOS providers and omit legacy image/platform requests.
Supply a JobDB URL reachable from the VM; Docker's loopback-host rewrite does not
apply to Tart or Orchard.

The pinned pulse-tart library exposes `GetCapabilities` with a complete configured
catalog and passes the selected concrete base to c2j through `--execution-base`.
The library resolves c2j during image preparation. Existing prepared images are
reused; upgrading Pulse does not reinstall c2j in those images.

## Docker in Tart/Orchard guests

Linux Docker needs no profiles, slot definitions, or worker endpoint. The
library provisions the integrated engine in the Linux job VM and reports its
`docker` capability through `GetCapabilities`.

For macOS Docker, set `docker: {}` and a backend `docker_endpoint`. The library
adds its default engine profile to the conventional macOS group. See
[tart-docker.yaml](../examples/tart-docker.yaml); no manual slots or profiles are
needed. Orchard accepts the same endpoint under `orchard`, using the worker's
guest-reachable IPv4 address. Reserve the port range for this provider owner on
that worker. The library validates the endpoint before provisioning.

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
[pulse-tart Docker contract](https://github.com/colony-2/pulse-tart/blob/ac235e01e7ca4b3bb65be8d39204d92985af4680/docs/docker-library.md)
for the tested Docker/Compose behavior and VM lifecycle.

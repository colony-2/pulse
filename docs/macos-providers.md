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

The default Tart pool has four independent slots, each with **4 vCPU and 8 GiB
RAM**. Zero CPU/memory in an explicit group selects these library defaults;
`count: 0` drains a group. Configure counts and sizes to fit the worker.

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

The default base is `macos-sequoia-15.7.7-arm64`, selectable with `macos*` or
`macos*arm64`. There are no aliases. It uses the image/version pair validated
by the pinned library release:

```text
ghcr.io/cirruslabs/macos-sequoia-base@sha256:eee659985b01e4df2a6c0754bd8d25e54969933c43fdcd4edef8bea337b3d354
```

The library prepares and verifies `nix`, `git`, `node`, `pnpm`, `uv`, and `python`
before making a template ready. The published default image's `admin` bootstrap
login is used only for this image; custom credentials use `ssh_password_env`.
Each job gets a fresh clone of a prepared, suspended slot. A busy or warming pool
returns `no_capacity`; leases are not queued while templates prepare.

## Configuration

Both provider types accept a strict `config` mapping:

| Field | Meaning |
| --- | --- |
| `owner_id` | Stable library ownership identity; defaults to a hash of provider type/name. |
| `state_dir` | Durable private journal directory; defaults under the OS user configuration directory at `pulse/providers/<identity>`. |
| `bootstrap_timeout` | Preparation deadline; default `45m`. |
| `bases` | Complete replacement base catalog; omitted uses the pinned default. |
| `slots` | Complete replacement slot groups; omitted uses four default slots. |
| `tart` | Local settings for `type: tart`. |
| `orchard` | Required connection settings for `type: orchard`. |

Each base accepts `name`, digest-pinned `image`, `os_version`,
`ssh_username` (default `admin`), `ssh_password_env`, optional
`ssh_host_key_file`, and `capabilities`. The library verifies the actual guest OS
version and supports the six capability probes listed above. Define slots with
`name`, `count`, concrete `base`, `cpu`, and `memory_mib`. Custom catalogs must
provide corresponding slots. Empty catalogs/groups are invalid.

Local `tart` settings are `binary` (default `tart` on `PATH`), `home` (default
`~/.tart`), `command_timeout`, and `ssh_timeout`. Orchard settings are `url`,
`worker`, optional `worker_labels`, `username`, `token_env`, `ca_cert_file`,
`request_timeout`, and `allow_http` for explicit lab HTTP connections. See
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

Pulse resolves `*` anywhere against configured concrete base names before
calling the library, preserving configured preference order. The
original assignment remains unchanged for fallback to other providers. A second
matching base is tried only after a definite capacity/constraint decline;
unknown or malformed responses never trigger another launch attempt.

This release runs macOS ARM64 guests. Scratch reservations, positive
infrastructure timeouts, start-before deadlines, and `defaults.env` are not
supported and are declined. Use c2j recipe timeouts. Capabilities and resource
requirements are passed to the library for admission and verification. The
legacy c2j image/platform compatibility path remains container-oriented: use
`defaults.base` for these macOS providers and omit legacy image/platform requests.
Supply a JobDB URL reachable from the VM; Docker's loopback-host rewrite does not
apply to Tart or Orchard.

The pinned pulse-tart library exposes `GetCapabilities` with a complete configured
catalog and passes the selected concrete base to c2j through `--execution-base`.
Its guest bundle pins a c2j worker that supports JobDB v2 leases and base demand
verification. Custom guest bundles must provide the same compatibility.

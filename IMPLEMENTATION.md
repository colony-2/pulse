# Implementation milestones

Each milestone receives its own tested commit.

1. Design and protocol baseline.
2. Portable compute types, allocation validation, priority-tier batch scheduler, cooldown.
3. Remote OpenAPI provider client and conformance tests.
4. c2j executable discovery, strict configuration, and controller.
5. Local Docker admission, container lifecycle, restart reconstruction, and supervisor.
6. Cloud Run, ECS, and Azure Container Apps Jobs adapters.
7. CLI/provider wiring, integration checks, operational documentation, and CI.

Cloud credentials and a Docker daemon are not assumed available in the development environment. Use fake HTTP/command endpoints for deterministic integration tests, and explicitly report which live checks were possible. The controller now embeds c2j’s public listing library by default, with optional external CLI listing. Executor containers continue to invoke c2j.

## Completed validation

The core, remote client, c2j/config/controller, Docker/supervisor, and cloud adapter milestones passed their targeted race-enabled tests before their commits. Final CLI wiring adds executable-to-remote integration, a real c2j/temporary JobDB discovery check, protocol validation, builds, and CI. See README.md for live-deployment checks still requiring infrastructure.

Implementation commits use `colony2.com <col2bot@colony2.com>` as requested.

## Public listing API migration

The controller uses `github.com/colony-2/c2j/pkg/joblist` by default. The dependency is pinned to a remotely resolvable pseudo-version containing the new API; Go 1.26 is now required. `c2j.mode: external` retains subprocess listing. Default images include Cortex and its supervisor; the optional `external-c2j` Docker target adds a CLI.

## Single-operation provider v1

All adapters implement `Submit` with complete resources, process, deadlines, and metadata. Cortex builds the environment from requested/defaulted values once and preserves it during fallback. Providers size and admit internally; no preparation API or plan token remains. The v1 OpenAPI and protocol examples are updated in place and checked by the schema validator. Regression coverage includes unchanged environments under provider rounding, partial fallback, uncertain submission failures, Docker capacity/replay, and both listing modes through the remote client.

## Native cloud credentials

Cloud adapters no longer invoke `aws`, `gcloud`, or `az`. Native SDKs handle Google ADC, AWS signing/credential discovery, and Azure environment/workload/managed identity authentication. Credential instances survive individual submission contexts for caching and refresh. The default distroless image supports all cloud adapters; a test-only target exercises native credentials and launch requests against fake endpoints in that runtime for AMD64 and ARM64.

## Active lists and public diagnostics

All five providers now implement paginated active instance listing, including queued/starting/running work and excluding terminal instances. Remote v1 uses `GET /v1/launches` with optional launch-ID filtering, replacing per-launch inspection. Terminal idempotency retention is unchanged.

Continuous mode serves public read-only HTTP endpoints for status, redacted configuration, all/provider-specific instances, cooldowns, and round-robin state. Provider reads have deadlines and bounded fan-out; aggregate failures preserve partial results. Tests cover native state filtering/pagination, remote wire validation, read-only scheduler snapshots, config redaction, partial failures, and real CLI startup/HTTP/SIGTERM shutdown. No persistent Cortex state is introduced.

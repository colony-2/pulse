# Implementation milestones

Each milestone receives its own tested commit.

1. Design and protocol baseline.
2. Portable compute types, allocation validation, priority-tier batch scheduler, cooldown.
3. Remote OpenAPI provider client and conformance tests.
4. c2j executable discovery, strict configuration, and controller.
5. Local Docker admission, container lifecycle, restart reconstruction, and supervisor.
6. Cloud Run, ECS, and Azure Container Apps Jobs adapters.
7. CLI/provider wiring, integration checks, operational documentation, and CI.

Cloud credentials and a Docker daemon are not assumed available in the development environment. Use fake HTTP/command endpoints for deterministic integration tests, and explicitly report which live checks were possible. c2j is an executable dependency, never an imported library.

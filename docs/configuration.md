# Configuration and container deployment

**Use `CORTEX_CONFIG` for simple container deployments.** Its value is the complete YAML configuration, with the same schema and validation used for files. Cortex parses it directly in Go, including in the distroless image.

## Source selection

| Priority | Source |
| --- | --- |
| 1 | File explicitly selected by `-config PATH`. |
| 2 | YAML contents in `CORTEX_CONFIG`, if the variable is set. |
| 3 | `cortex.yaml` in the working directory. |

Only one source is used. There is no merging, and a selected source's failure does not trigger fallback. A set-but-empty or whitespace-only `CORTEX_CONFIG` is an error; unset it to use the default file. An explicit file ignores the inline value, even if that value is invalid. An explicit missing file fails instead of using the environment.

The value is raw YAML, not a filename, URL, or base64 string. Use real newlines for multiline YAML. Cortex performs no shell expansion or `${VARIABLE}` interpolation. Quoting and expansion performed by your host shell or deployment tooling happen before Cortex receives the value.

Configuration is read once at startup. Restart or deploy a new revision after changes. `-check` and `-once` use the same source selection; `-version` needs no configuration. `/config` shows the parsed, redacted configuration regardless of its source, never the raw environment document. See [HTTP API](http-api.md).

## Docker or a native executable

Copy [container.yaml](../examples/container.yaml), edit the deployment settings, then load its contents on the host:

```sh
export CORTEX_CONFIG="$(cat cortex.yaml)"
cortex -check
cortex
```

For Docker, pass the environment variable by name:

```sh
docker run --rm --init --read-only --tmpfs /tmp -p 8080:8080 \
  -e CORTEX_CONFIG -e CORTEX_PROVIDER_TOKEN \
  ghcr.io/colony-2/cortex:latest
```

The host file is just an authoring convenience. You can instead store the entire YAML directly in your deployment's environment settings. The container needs no configuration mount, shell, or startup script. Credentials remain in separate secret/environment settings or cloud identities; fields such as `token_env` and `jobdb_token_env` name those variables.

For example, a shell can set the document directly using single quotes, which preserve literal dollar signs and newlines:

```sh
export CORTEX_CONFIG='
defaults:
  image: registry.example/c2j-runner:1.0
  platform: linux/amd64
  cpu: "1"
  memory: 1Gi
  scratch: 1Gi
providers:
  runners:
    type: remote
    endpoint: https://runners.example.com
    token_env: CORTEX_PROVIDER_TOKEN
targets:
  - instance_id: production
    jobdb: https://jobdb.example.com/acme
    cells: [github.com/acme/app]
    launch_services: [{name: runners, priority: 1}]
'
```

## Google Cloud Run

For a simple Cloud Run service deployment, add `CORTEX_CONFIG` under **Containers → Variables & Secrets**, with the complete YAML as its value. Leave container arguments unset so an explicit `-config` does not override it. Environment settings belong to the service revision. Cloud Run limits each environment variable to 32 KB; larger configurations can use a mounted file. See [Cloud Run environment configuration](https://docs.cloud.google.com/run/docs/configuring/services/environment-variables).

If you already store the document in Secret Manager, Cloud Run can inject a selected version as the `CORTEX_CONFIG` environment variable instead of mounting a file. See [secret injection](https://docs.cloud.google.com/run/docs/configuring/services/secrets). Use the controller's attached service account for cloud API access as described in [cloud authentication](cloud-authentication.md).

Cortex's continuous polling needs **instance-based billing and at least one minimum instance** so CPU remains available without HTTP traffic. See [background processing settings](https://docs.cloud.google.com/run/docs/configuring/billing-settings). Leave `http.listen` unset to use Cloud Run's `PORT` value.

## Mounted-file alternative

File configuration remains available for larger documents and deployments that already manage mounts:

```sh
docker run --rm --init --read-only --tmpfs /tmp -p 8080:8080 \
  --mount "type=bind,source=$PWD/cortex.yaml,target=/etc/cortex/cortex.yaml,readonly" \
  -e CORTEX_PROVIDER_TOKEN \
  ghcr.io/colony-2/cortex:latest -config /etc/cortex/cortex.yaml
```

The file must be readable by UID/GID `65532:65532`. The image no longer supplies `-config /etc/cortex/cortex.yaml` as default arguments, so existing deployments using that mounted path must add the explicit arguments shown above. No source is baked into the image; environment-only deployments start it without arguments.

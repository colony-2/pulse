#!/usr/bin/env bash
set -euo pipefail
image="${1:?usage: smoke-container.sh IMAGE VERSION PLATFORM}"
version="${2:?expected Cortex version}"
platform="${3:?container platform}"
[[ "$(docker run --rm --platform "$platform" "$image" -version)" == "cortex $version" ]]
# The default image accepts inline configuration without a mounted file.
[[ "$(docker image inspect --format '{{len .Config.Cmd}}' "$image")" == 0 ]]
docker run --rm --platform "$platform" --read-only --tmpfs /tmp \
  -e CORTEX_CONFIG="$(cat examples/container.yaml)" \
  -e CORTEX_PROVIDER_TOKEN=smoke-test "$image" -check
# An explicit file takes precedence even when inline configuration is invalid.
docker run --rm --platform "$platform" --read-only --tmpfs /tmp \
  -e CORTEX_CONFIG='[invalid' \
  --mount "type=bind,source=$PWD/examples/container.yaml,target=/etc/cortex/cortex.yaml,readonly" \
  -e CORTEX_PROVIDER_TOKEN=smoke-test "$image" -config /etc/cortex/cortex.yaml -check
docker run --rm --platform "$platform" --entrypoint /usr/local/bin/cortex-exec "$image" \
  --timeout 10s -- /usr/local/bin/cortex -version
[[ "$(docker image inspect --format '{{.Config.User}}' "$image")" == "65532:65532" ]]
# The default image must not depend on or include a c2j executable.
if docker run --rm --platform "$platform" --entrypoint /usr/local/bin/c2j "$image" version; then
  echo 'Unexpected c2j executable in default image' >&2
  exit 1
fi

# The same runtime must authenticate and submit using native cloud SDKs, with
# no cloud CLI or writable root filesystem. Fake token/metadata/API endpoints
# run inside the test process; this does not access real cloud accounts.
docker buildx build --platform "$platform" --target cloud-smoke --load \
  --build-arg "VERSION=$version" --tag "$image-cloud-smoke" .
docker run --rm --platform "$platform" --read-only --tmpfs /tmp \
  "$image-cloud-smoke"

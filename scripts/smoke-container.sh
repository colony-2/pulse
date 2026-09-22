#!/usr/bin/env bash
set -euo pipefail
image="${1:?usage: smoke-container.sh IMAGE VERSION PLATFORM}"
version="${2:?expected Cortex version}"
platform="${3:?container platform}"
[[ "$(docker run --rm --platform "$platform" "$image" -version)" == "cortex $version" ]]
docker run --rm --platform "$platform" --read-only --tmpfs /tmp \
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

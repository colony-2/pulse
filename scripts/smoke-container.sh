#!/usr/bin/env bash
set -euo pipefail
image="${1:?usage: smoke-container.sh IMAGE VERSION PLATFORM C2J_VERSION}"
version="${2:?expected Cortex version}"
platform="${3:?container platform}"
c2j_version="${4:?expected c2j tag}"
[[ "$(docker run --rm --platform "$platform" "$image" -version)" == "cortex $version" ]]
[[ "$(docker run --rm --platform "$platform" --entrypoint /usr/local/bin/c2j "$image" version)" == "c2j version ${c2j_version#v}" ]]
docker run --rm --platform "$platform" --read-only --tmpfs /tmp \
  --mount "type=bind,source=$PWD/examples/container.yaml,target=/etc/cortex/cortex.yaml,readonly" \
  -e CORTEX_PROVIDER_TOKEN=smoke-test "$image" -config /etc/cortex/cortex.yaml -check
docker run --rm --platform "$platform" --entrypoint /usr/local/bin/cortex-exec "$image" \
  --timeout 10s -- /usr/local/bin/c2j version
[[ "$(docker image inspect --format '{{.Config.User}}' "$image")" == "65532:65532" ]]
[[ "$(docker image inspect --format '{{index .Config.Labels "com.colony2.c2j.version"}}' "$image")" == "$c2j_version" ]]

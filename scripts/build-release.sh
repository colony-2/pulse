#!/usr/bin/env bash
set -euo pipefail
version="${1:?usage: build-release.sh VERSION [GOOS/GOARCH ...]}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Expected MAJOR.MINOR.PATCH' >&2; exit 1; }
shift
if [[ $# == 0 ]]; then
  set -- linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
fi
for target in "$@"; do
  case "$target" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;;
    *) echo "Unsupported release target: $target" >&2; exit 1 ;;
  esac
  goos="${target%/*}"
  goarch="${target#*/}"
  out="dist/build/$goos-$goarch"
  mkdir -p "$out"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X main.version=$version" -o "$out/pulse" ./cmd/pulse
  if [[ "$target" == darwin/arm64 && "$(uname -s)" == Darwin ]]; then
    codesign --force --sign - --entitlements cmd/pulse/vz.entitlements "$out/pulse"
  fi
  # The helper executes inside Linux containers, including with Docker Desktop.
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath -buildvcs=false \
    -ldflags '-s -w' -o "$out/pulse-exec" ./cmd/pulse-exec
done

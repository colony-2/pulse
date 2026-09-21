#!/usr/bin/env bash
set -euo pipefail
version="${1:?usage: build-release.sh VERSION}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Expected MAJOR.MINOR.PATCH' >&2; exit 1; }
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  out="dist/build/$goos-$goarch"
  mkdir -p "$out"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X main.version=$version" -o "$out/cortex" ./cmd/cortex
  if [[ "$goos" == linux ]]; then
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath -buildvcs=false \
      -ldflags '-s -w' -o "$out/cortex-exec" ./cmd/cortex-exec
  fi
done

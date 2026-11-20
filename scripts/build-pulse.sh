#!/usr/bin/env bash
set -euo pipefail

output="${1:?usage: build-pulse.sh OUTPUT or build-pulse.sh --install}"
if [[ "$output" != --install && "$output" != /* ]]; then
  output="$PWD/$output"
fi
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
if [[ "$output" == --install ]]; then
  go install ./cmd/pulse
  output=$(go list -f '{{.Target}}' ./cmd/pulse)
else
  go build -o "$output" ./cmd/pulse
fi

host_os=$(uname -s)
if [[ "$host_os" != Darwin ]]; then
  exit 0
fi
target_os=$(go env GOOS)
target_arch=$(go env GOARCH)
if [[ "$target_os/$target_arch" != darwin/arm64 ]]; then
  exit 0
fi

codesign --force --sign - --entitlements cmd/pulse/vz.entitlements "$output"
codesign --verify --strict "$output"
if ! virtualization=$(codesign --display --entitlements - --xml "$output" \
  | plutil -extract 'com\.apple\.security\.virtualization' raw -expect bool -o - -) \
  || [[ "$virtualization" != true ]]; then
  echo "Expected com.apple.security.virtualization=true in the signature for $output" >&2
  exit 1
fi

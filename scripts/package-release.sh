#!/usr/bin/env bash
set -euo pipefail
version="${1:?usage: package-release.sh VERSION}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Expected MAJOR.MINOR.PATCH' >&2; exit 1; }
mkdir -p dist/release
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  case "$goos" in linux) os_name=Linux ;; darwin) os_name=Darwin ;; esac
  case "$goarch" in amd64) arch_name=x86_64 ;; arm64) arch_name=arm64 ;; esac
  mkdir -p "$stage/$target"
  cp dist/build/"$goos-$goarch"/* "$stage/$target/"
  cp README.md LICENSE "$stage/$target/"
  cp -R examples docs "$stage/$target/"
  # Name the binary explicitly for the npm installer, not ./cortex.
  tar -C "$stage/$target" -czf "dist/release/cortex_${version}_${os_name}_${arch_name}.tar.gz" cortex README.md LICENSE examples docs $(if [[ "$goos" == linux ]]; then echo cortex-exec; fi)
done
cp -R npm/cortex "$stage/npm"
cp README.md LICENSE "$stage/npm/"
node - "$stage/npm/package.json" "$version" <<'NODE'
const fs = require('node:fs');
const [file, version] = process.argv.slice(2);
const pkg = JSON.parse(fs.readFileSync(file));
pkg.version = version;
fs.writeFileSync(file, JSON.stringify(pkg, null, 2) + '\n');
NODE
npm pack "$stage/npm" --pack-destination dist/release
(cd dist/release && sha256sum ./*.tar.gz ./*.tgz | sed 's|  ./|  |' | sort -k2 > checksums.txt)

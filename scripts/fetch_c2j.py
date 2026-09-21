#!/usr/bin/env python3
"""Resolve and checksum-verify official c2j Linux release binaries."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import tarfile
import urllib.request

REPO = "https://github.com/colony-2/c2j"


def download(url):
    request = urllib.request.Request(url, headers={"User-Agent": "cortex-release"})
    with urllib.request.urlopen(request, timeout=120) as response:
        if not response.url.startswith("https://"):
            raise ValueError("c2j downloads require HTTPS")
        return response.read()


def resolve(version, fetch=download):
    if version == "latest":
        release = json.loads(fetch("https://api.github.com/repos/colony-2/c2j/releases/latest"))
        if release.get("draft") or release.get("prerelease"):
            raise ValueError("Expected a stable c2j release")
        version = release["tag_name"]
    if not re.fullmatch(r"v\d+\.\d+\.\d+", version):
        raise ValueError("c2j version must be latest or vMAJOR.MINOR.PATCH")
    return version


def install(version, arch, output, fetch=download):
    version = resolve(version, fetch)
    target = {"amd64": "x86_64", "arm64": "arm64"}[arch]
    name = f"c2j_{version[1:]}_Linux_{target}.tar.gz"
    base = f"{REPO}/releases/download/{version}"
    sums = fetch(f"{base}/checksums.txt").decode().splitlines()
    matches = [line.split()[0] for line in sums if len(line.split()) == 2 and line.split()[1] == name]
    if len(matches) != 1 or not re.fullmatch(r"[a-f0-9]{64}", matches[0]):
        raise ValueError(f"Missing or ambiguous c2j checksum for {name}")
    archive = fetch(f"{base}/{name}")
    if hashlib.sha256(archive).hexdigest() != matches[0]:
        raise ValueError(f"Checksum mismatch for {name}")
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    # Read only known regular files; never extract archive paths or symlinks.
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as tar:
        for name in ("c2j", "LICENSE"):
            member = tar.getmember(name)
            if not member.isfile():
                raise ValueError(f"c2j archive contains invalid {name}")
            target = output / ("c2j" if name == "c2j" else "C2J-LICENSE")
            target.write_bytes(tar.extractfile(member).read())
            target.chmod(0o755 if name == "c2j" else 0o644)
    (output / "c2j-version.txt").write_text(version + "\n")
    return version


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", default="latest")
    parser.add_argument("--arch", choices=("amd64", "arm64"))
    parser.add_argument("--output", default="dist/c2j")
    args = parser.parse_args()
    if args.arch:
        print(install(args.version, args.arch, args.output))
    else:
        print(resolve(args.version))

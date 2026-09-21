# Packaging and releases

Cortex follows [c2j's GitHub Actions release pattern](https://github.com/colony-2/c2j/blob/main/.github/workflows/release.yaml): test, bump a version on `main`, build native Go executables, optionally sign macOS binaries, publish GitHub assets, and publish an npm wrapper. Cortex additionally publishes a distroless container for both Linux architectures.

## Release outputs

For `vX.Y.Z`:

| Output | Location / name |
| --- | --- |
| Linux AMD64 executable and supervisor | `cortex_X.Y.Z_Linux_x86_64.tar.gz` |
| Linux ARM64 executable and supervisor | `cortex_X.Y.Z_Linux_arm64.tar.gz` |
| macOS Intel executable | `cortex_X.Y.Z_Darwin_x86_64.tar.gz` |
| macOS Apple Silicon executable | `cortex_X.Y.Z_Darwin_arm64.tar.gz` |
| npm package | `colony2-cortex-X.Y.Z.tgz`, also published as `@colony2/cortex@X.Y.Z` |
| Multi-architecture image | `ghcr.io/colony-2/cortex:vX.Y.Z` |
| Architecture-specific images | `ghcr.io/colony-2/cortex:vX.Y.Z-amd64` and `:vX.Y.Z-arm64` |
| Container archives | `cortex_X.Y.Z_container_Linux_amd64.tar.gz` and `_arm64.tar.gz` |
| Verification and identity | `checksums.txt`, `container-image.txt`, `versions.txt` |

All listed files are attached to the GitHub release. The container archives use Docker's save format and support `docker load --input`. `container-image.txt` records the registry manifest digest. `checksums.txt` covers the executable archives, npm tarball, container archives, and version/manifest records.

The npm wrapper installs the native Cortex binary only. It verifies the downloaded GitHub archive against the release checksums, extracts the executable, and atomically installs it under the package's `vendor` directory. Failed checksum verification preserves an existing installation. The launcher forwards command-line arguments, exit codes, and termination signals. Install scripts, `tar`, and access to GitHub download endpoints are required; installations with scripts disabled can run `npm rebuild @colony2/cortex` after enabling them.

## Pipeline

The workflow is [.github/workflows/release.yaml](../.github/workflows/release.yaml).

1. Reuse the test workflow: Go race/vet/build checks across Linux AMD64/ARM64 and macOS, npm/Python tests, OpenAPI validation, cross-platform package builds, and both container architectures.
2. Use the same `anothrNick/github-tag-action` version and patch-bump default as c2j. Main-branch releases use `vMAJOR.MINOR.PATCH` tags.
3. Build static Go executables and optionally sign/notarize both Darwin binaries before packaging.
4. Pack the npm tarball and smoke-test its installer against the local, checksum-verified release archives.
5. Resolve the latest **stable published c2j release once**, download and verify that release for both architectures, then build and smoke-test both non-root distroless images. Build arguments and image records retain the selected c2j version.
6. Publish the versioned images and multi-architecture manifest to GHCR; attach all artifacts to GitHub Releases.
7. Test npm installation using the real published GitHub download URLs, then publish the **same tarball** attached to GitHub. This order ensures the npm installer can already fetch its binary.
8. Promote the successful image release to `latest` after npm publishing succeeds.

Release runs are serialized and are not canceled by new pushes. The workflow can also be dispatched manually; supply an existing tag to rebuild that release, or leave it empty on `main` to create one. Re-running **failed jobs** is preferable after a transient publish failure: it preserves the already-produced artifacts. Rebuilding an existing tag can select a newer c2j release or base image, so published image digests should be used when exact identity matters. npm versions cannot be overwritten; a full rebuild after npm publication requires a new version.

No tags, npm packages, or images are published by local test commands. Pushing to this repository's `main` branch starts the release workflow once it is hosted on GitHub and configured below.

## Repository setup

### GitHub / GHCR

- Use the `colony-2/cortex` repository. Automatic publishing is restricted to the `colony-2` owner.
- Allow the workflow's `GITHUB_TOKEN` to create tags/releases (`contents: write`) and publish images (`packages: write`). Grant the repository access if a pre-existing GHCR package is owned elsewhere.
- Set the GHCR package visibility to **public** for anonymous pulls. Workflow permission to push does not itself change package visibility.
- Configure the npm scope/package below before the first release.

### npm

Publish to `@colony2/cortex`, following c2j's two supported authentication modes:

1. **Trusted publishing:** configure the package's trusted publisher with owner `colony-2`, repository `cortex`, and workflow filename `release.yaml`. The job has `id-token: write`, Node 24, and npm 11, satisfying npm's OIDC requirements. See [npm trusted publishing](https://docs.npmjs.com/trusted-publishers/).
2. **Token publishing:** set repository secret `NPM_TOKEN` to a token with publish access to `@colony2/cortex`. This is also the bootstrap option if the package does not yet exist and trusted publishing is not configured. The workflow supplies it as `NODE_AUTH_TOKEN` only to the publishing step.

Both paths request provenance and public access. The npm package name and GitHub download repository are fixed to Cortex, so forks must update the package metadata, installer URL, image labels, and workflow owner guard before publishing their own package.

### Optional macOS signing

The c2j signing convention is retained. Configure either all of these secrets or none:

- `MACOS_SIGN_P12`
- `MACOS_SIGN_PASSWORD`
- `APPLE_API_ISSUER`
- `APPLE_API_KEY_ID`
- `APPLE_API_KEY`

With all five present, Quill v0.7.1 signs and notarizes Darwin binaries before their checksums and npm packages are produced. Partial configuration fails the release. Without secrets, the archives contain unsigned binaries; macOS policy may require users to approve downloaded executables.

## Container composition

[Dockerfile](../Dockerfile) cross-compiles Cortex and its supervisor with CGO disabled, and obtains the official c2j binary through [fetch_c2j.py](../scripts/fetch_c2j.py). Downloaded c2j archives must have a matching SHA-256 entry in the same release's `checksums.txt`. The final stage is `gcr.io/distroless/static-debian12:nonroot`, with no Go compiler, Python, shell, Git, or cloud SDKs.

The image uses the latest stable c2j available **at build time**, not the head of c2j's source branch and not a runtime downloader. Release builds pass an explicit `C2J_VERSION=vX.Y.Z` to both architectures. Local builds may omit it to resolve `latest`, but should pass the resolved tag explicitly to keep cache invalidation and metadata accurate. The exact tag is always saved at `/usr/share/cortex/c2j-version.txt`; the label is exact when an explicit version is supplied.

Remote providers work without external command-line tools. Cloud Run and Azure can use configured access-token environment variables; tokens must remain valid over the controller's lifetime. ECS requires AWS CLI v2 in a deployment-specific image. Executor job images are configured independently and must supply their recipe dependencies.

### Docker provider from a container

A containerized Cortex controlling a host Docker daemon additionally needs:

- The daemon socket mounted and accessible to the controller UID or supplementary socket group.
- A host copy of the Linux `cortex-exec` helper, mounted at the **same absolute path** in the controller and configured as `providers.<name>.helper`. Merely having `/usr/local/bin/cortex-exec` inside the controller image does not make that path available to the host daemon's bind mounts.
- A shared host directory for `providers.<name>.lock_dir`, mounted identically by every controller that could reach this daemon. Private container `/tmp` directories cannot enforce a single admission owner across controllers.
- Capacity budgets reserving headroom for the host, controller, and other workloads.

## Local validation

```sh
make test vet build test-packaging
scripts/build-release.sh 0.0.0
scripts/package-release.sh 0.0.0
node scripts/smoke-npm.js 0.0.0
```

With Docker and Buildx available:

```sh
C2J_VERSION=$(python3 scripts/fetch_c2j.py)
docker buildx build --load --platform linux/arm64 \
  --build-arg VERSION=0.0.0 --build-arg "C2J_VERSION=$C2J_VERSION" \
  -t cortex:test-arm64 .
scripts/smoke-container.sh cortex:test-arm64 0.0.0 linux/arm64 "$C2J_VERSION"
```

Repeat for `linux/amd64`; execution on a different host architecture requires QEMU/binfmt support. CI configures this automatically. The smoke check runs both executable versions, validates Cortex configuration using bundled c2j on a read-only filesystem, exercises the supervisor, and checks the non-root image user.

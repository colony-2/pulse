# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.1
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS cortex-build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=$VERSION" -o /out/cortex ./cmd/cortex && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags '-s -w' -o /out/cortex-exec ./cmd/cortex-exec
RUN go list -m -f '{{.Version}}' github.com/colony-2/c2j > /out/c2j-version.txt && \
    cp "$(go list -m -f '{{.Dir}}' github.com/colony-2/c2j)/LICENSE" /out/C2J-LICENSE

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="Cortex" \
      org.opencontainers.image.description="Container compute orchestration for c2j jobs" \
      org.opencontainers.image.source="https://github.com/colony-2/cortex" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION"
COPY --from=cortex-build /out/cortex /out/cortex-exec /usr/local/bin/
COPY --from=cortex-build /out/C2J-LICENSE /out/c2j-version.txt /usr/share/cortex/
COPY LICENSE /usr/share/cortex/LICENSE
ENV PATH=/usr/local/bin:/usr/bin:/bin
WORKDIR /home/nonroot
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cortex"]
CMD []

# Exercise native cloud authentication in the same shell-free runtime in CI.
# This test executable is not included in either published image target.
FROM cortex-build AS cloud-smoke-build
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go test -c \
      -o /out/cloud.test ./internal/providers/cloud

FROM runtime AS cloud-smoke
COPY --from=cloud-smoke-build /out/cloud.test /usr/local/bin/cloud.test
ENTRYPOINT ["/usr/local/bin/cloud.test"]
CMD ["-test.v"]

# Optional compatibility image; the default target below remains minimal.
FROM --platform=$BUILDPLATFORM python:3.13-slim-bookworm AS c2j-download
ARG TARGETARCH
ARG C2J_VERSION=latest
COPY scripts/fetch_c2j.py /fetch_c2j.py
RUN python /fetch_c2j.py --version "$C2J_VERSION" --arch "$TARGETARCH" --output /out

FROM runtime AS external-c2j
ARG C2J_VERSION=latest
LABEL com.colony2.c2j.executable.version="$C2J_VERSION"
COPY --from=c2j-download /out/c2j /usr/local/bin/c2j
COPY --from=c2j-download /out/C2J-LICENSE /usr/share/cortex/C2J-EXECUTABLE-LICENSE
COPY --from=c2j-download /out/c2j-version.txt /usr/share/cortex/c2j-executable-version.txt

FROM runtime AS final

# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.1
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS pulse-build
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
      -ldflags "-s -w -X main.version=$VERSION" -o /out/pulse ./cmd/pulse && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags '-s -w' -o /out/pulse-exec ./cmd/pulse-exec
RUN go list -m -f '{{.Version}}' github.com/colony-2/c2j > /out/c2j-version.txt && \
    cp "$(go list -m -f '{{.Dir}}' github.com/colony-2/c2j)/LICENSE" /out/C2J-LICENSE

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="Pulse" \
      org.opencontainers.image.description="Container compute orchestration for c2j jobs" \
      org.opencontainers.image.source="https://github.com/colony-2/pulse" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION"
COPY --from=pulse-build /out/pulse /out/pulse-exec /usr/local/bin/
COPY --from=pulse-build /out/C2J-LICENSE /out/c2j-version.txt /usr/share/pulse/
COPY LICENSE /usr/share/pulse/LICENSE
ENV PATH=/usr/local/bin:/usr/bin:/bin
WORKDIR /home/nonroot
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/pulse"]
CMD []

# Exercise native cloud authentication in the same shell-free runtime in CI.
# This test executable is not included in the published image.
FROM pulse-build AS cloud-smoke-build
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go test -c \
      -o /out/cloud.test ./internal/providers/cloud

FROM runtime AS cloud-smoke
COPY --from=cloud-smoke-build /out/cloud.test /usr/local/bin/cloud.test
ENTRYPOINT ["/usr/local/bin/cloud.test"]
CMD ["-test.v"]

FROM runtime AS final

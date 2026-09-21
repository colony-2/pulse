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

FROM --platform=$BUILDPLATFORM python:3.13-slim-bookworm AS c2j-download
ARG TARGETARCH
ARG C2J_VERSION=latest
COPY scripts/fetch_c2j.py /fetch_c2j.py
RUN python /fetch_c2j.py --version "$C2J_VERSION" --arch "$TARGETARCH" --output /out

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG REVISION=unknown
ARG C2J_VERSION=latest
LABEL org.opencontainers.image.title="Cortex" \
      org.opencontainers.image.description="Container compute orchestration for c2j jobs" \
      org.opencontainers.image.source="https://github.com/colony-2/cortex" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION" \
      com.colony2.c2j.version="$C2J_VERSION"
COPY --from=cortex-build /out/cortex /out/cortex-exec /usr/local/bin/
COPY --from=c2j-download /out/c2j /usr/local/bin/c2j
COPY --from=c2j-download /out/C2J-LICENSE /out/c2j-version.txt /usr/share/cortex/
COPY LICENSE /usr/share/cortex/LICENSE
ENV PATH=/usr/local/bin:/usr/bin:/bin
WORKDIR /home/nonroot
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/cortex"]
CMD ["-config", "/etc/cortex/cortex.yaml"]

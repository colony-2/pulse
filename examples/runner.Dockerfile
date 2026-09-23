# Build from the repository root: docker build -f examples/runner.Dockerfile -t pulse-runner:local .
FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/pulse-exec ./cmd/pulse-exec
ARG C2J_COMMIT=5a2b646395a29ed80af4a08935fef1095838564f
RUN git clone https://github.com/colony-2/c2j.git /c2j && git -C /c2j checkout "$C2J_COMMIT"
WORKDIR /c2j
RUN CGO_ENABLED=0 go build -o /out/c2j ./cmd/c2j

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/c2j /out/pulse-exec /usr/local/bin/
RUN mkdir -p /scratch
WORKDIR /scratch
ENV TMPDIR=/scratch
ENTRYPOINT ["c2j"]

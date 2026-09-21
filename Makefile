.PHONY: build test vet
build:
	mkdir -p bin
	go build -o bin/cortex ./cmd/cortex
	CGO_ENABLED=0 go build -o bin/cortex-exec ./cmd/cortex-exec

test:
	go test -race ./...

vet:
	go vet ./...

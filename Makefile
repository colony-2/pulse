.PHONY: build test vet test-packaging
build:
	mkdir -p bin
	go build -o bin/pulse ./cmd/pulse
	CGO_ENABLED=0 go build -o bin/pulse-exec ./cmd/pulse-exec

test:
	go test -race ./...

vet:
	go vet ./...

test-packaging:
	node --test scripts/npm.test.js

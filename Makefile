.PHONY: build test vet test-packaging
build:
	mkdir -p bin
	scripts/build-pulse.sh bin/pulse
	CGO_ENABLED=0 GOOS=linux go build -o bin/pulse-exec ./cmd/pulse-exec

test:
	go test -race -timeout=45m ./...

vet:
	go vet ./...

test-packaging:
	node --test scripts/npm.test.js

.PHONY: build test vet test-packaging
build:
	mkdir -p bin
	go build -o bin/cortex ./cmd/cortex
	CGO_ENABLED=0 go build -o bin/cortex-exec ./cmd/cortex-exec

test:
	go test -race ./...

vet:
	go vet ./...

test-packaging:
	node --test scripts/npm.test.js
	python3 -m unittest discover -s scripts -p 'test_*.py'

.PHONY: build test check

build:
	go build -buildvcs=false -trimpath -o bin/taskctl ./cmd/taskctl

test:
	go test ./...

check:
	go test ./...
	go vet ./...

.PHONY: build test run clean

# Release identity injected into the binary at link time. An exact git tag
# wins (v0.2.0, or v0.2.0-dirty); otherwise development builds report
# devel-<commit>. Override for release candidates: make build VERSION=v0.2.0-rc1
VERSION ?= $(shell git describe --tags --exact-match --dirty 2>/dev/null || printf 'devel-%s' "$$(git describe --always --dirty 2>/dev/null)")

build:
	CGO_ENABLED=0 go build -ldflags "-X github.com/lsm/dolmen/internal/version.Version=$(VERSION)" -o dolmen .

test:
	go vet ./... && go test ./...

run:
	go run . -addr 127.0.0.1:8790 -data ./data

clean:
	rm -f dolmen

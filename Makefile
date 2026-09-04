.PHONY: build test race run clean release release-sbom release-checksums release-all image vulncheck

# Release identity injected into the binary at link time. An exact git tag
# wins (v0.2.0, or v0.2.0-dirty); otherwise development builds report
# devel-<commit>, or devel-unknown outside a git checkout. Override for
# release candidates: make build VERSION=v0.2.0-rc1
VERSION ?= $(shell git describe --tags --exact-match --dirty 2>/dev/null || printf 'devel-%s' "$$(git describe --always --dirty 2>/dev/null || echo unknown)")

LDFLAGS := -X github.com/lsm/dolmen/internal/version.Version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64
RELEASE_DIR := dist

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dolmen .

test:
	go vet ./... && go test ./...

race:
	go vet ./... && go test -race ./...

run:
	go run . -addr 127.0.0.1:8790 -data ./data

clean:
	rm -rf dolmen dolmen.exe $(RELEASE_DIR)

release: clean
	@mkdir -p $(RELEASE_DIR)
	@for p in $(PLATFORMS); do \
		goos=$${p%%/*}; \
		goarch=$${p#*/}; \
		ext=""; \
		[ "$$goos" = "windows" ] && ext=".exe"; \
		out="$(RELEASE_DIR)/dolmen-$(VERSION)-$$goos-$$goarch$$ext"; \
		echo "Building $$out"; \
		CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -trimpath -ldflags "$(LDFLAGS)" -o "$$out" . || exit 1; \
	done

release-sbom:
	@command -v syft >/dev/null 2>&1 || { echo "syft not found; install from https://github.com/anchore/syft"; exit 1; }
	@mkdir -p $(RELEASE_DIR)
	@tmp=$$(mktemp); \
	trap 'rm -f "$$tmp"' EXIT; \
	syft . -o spdx-json=$$tmp && \
	mv "$$tmp" $(RELEASE_DIR)/dolmen-$(VERSION)-sbom.spdx.json

CHECKSUM := $(shell if command -v sha256sum >/dev/null 2>&1; then echo sha256sum; else echo shasum -a 256; fi)

release-checksums:
	@cd $(RELEASE_DIR) && { \
		for f in *; do \
			[ "$$f" = "SHA256SUMS" ] && continue; \
			[ -f "$$f" ] && { $(CHECKSUM) "$$f" || exit 1; }; \
		done; \
	} > .SHA256SUMS.new && mv .SHA256SUMS.new SHA256SUMS

release-all:
	@$(MAKE) release
	@$(MAKE) release-sbom
	@$(MAKE) release-checksums

image:
	docker buildx build \
		--build-arg VERSION=$(VERSION) \
		-t ghcr.io/lsm/dolmen:$(VERSION) \
		--load .

vulncheck:
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not found; install with: go install golang.org/x/vuln/cmd/govulncheck@latest"; exit 1; }
	CGO_ENABLED=0 govulncheck ./...

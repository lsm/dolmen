# Test suites (every change)

What CI's `test`, `cgo-free-build` and `no-comments` jobs run
(`.github/workflows/ci.yml`), from the repo root:

```bash
go vet ./...
CGO_ENABLED=1 go test -race ./...
(cd spike/graph-index && go test -race ./...)
(cd examples/basic && go build ./... && go vet ./... && go run .)
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./...
go run github.com/lsm/nocomment-for-agents/go@016ad219e84b9b78ca66ff1b66a7729218030c12 --check
```

`make test` is the quick loop (vet + tests without `-race`); concurrency
changes must pass `make race`. `make vulncheck` needs `govulncheck` on PATH.

Narrower loops while iterating:

```bash
go test ./internal/conformance   # /v1 + MCP + facade contract; update it in any behavior-changing PR
go test ./internal/blackbox      # builds the binary and drives it as a subprocess; run the whole package
go test ./internal/store -run TestName
```

Expect: every package `ok`, `no-comments` silent with exit 0, and the example
printing its results with exit 0.

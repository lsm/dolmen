#!/usr/bin/env bash
set -euo pipefail
prefix="${1:?usage: graph-checks.sh OUTPUT_PREFIX}"
export TMPDIR="$(cd "${TMPDIR:-/tmp}" && pwd -P)"
run() {
  local name="$1"
  shift
  "$@" >"$prefix-$name.log" 2>&1 || { cat "$prefix-$name.log"; return 1; }
  echo "PASS $name ($prefix-$name.log)"
}
run vet go vet ./...
run race env CGO_ENABLED=1 go test -race ./...
run spike bash -c 'cd spike/graph-index && go test -race ./...'
run example bash -c 'cd examples/basic && go build ./... && go vet ./... && go run .'
run pure-build env CGO_ENABLED=0 go build ./...
run pure-vet env CGO_ENABLED=0 go vet ./...
run comments go run github.com/lsm/nocomment-for-agents/go@016ad219e84b9b78ca66ff1b66a7729218030c12 --check
run vulnerability go run golang.org/x/vuln/cmd/govulncheck@latest ./...

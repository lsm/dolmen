#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
: "${1:?usage: run.sh /absolute/owned-evidence-directory}"
case "$1" in /*) ;; *) echo 'use an absolute evidence directory' >&2; exit 1 ;; esac
mkdir -p "$1"
export GOTOOLCHAIN=go1.26.10 GOMAXPROCS=4
CGO_ENABLED=0 go build -trimpath -o "$1/graphbench" ./cmd/graphbench
{
  git rev-parse HEAD
  go version
  uname -sm
  if [ "$(uname -s)" = Darwin ]; then sysctl hw.memsize hw.logicalcpu machdep.cpu.brand_string; fi
} > "$1/host.txt"
for scale in 10000 100000; do
  for backend in sqlite badger; do
    "$1/graphbench" -backend "$backend" -data "$1/data" -nodes "$scale" -edges "$((scale * 10))" -samples 100 > "$1/$backend-$scale.json"
  done
done
python3 compare.py "$1"

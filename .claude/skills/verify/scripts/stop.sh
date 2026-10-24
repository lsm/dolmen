#!/usr/bin/env bash
# Stop the server serve.sh started and remove its work dir.
# Usage: stop.sh <work dir>
set -euo pipefail
W="$1"
case "$W" in *dolmen-verify.*) ;; *) echo "refusing: $W is not a serve.sh work dir" >&2; exit 2 ;; esac
if [ -f "$W/pid" ]; then kill "$(cat "$W/pid")" 2>/dev/null || true; fi
for _ in $(seq 1 50); do kill -0 "$(cat "$W/pid")" 2>/dev/null || break; sleep 0.2; done
rm -rf "$W"

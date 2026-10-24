#!/usr/bin/env bash
# Build ./cmd/dolmen into a fresh work dir and start it on loopback with
# synthetic-only state: an empty data dir, no embedding model, auth off.
# Usage: serve.sh [extra dolmen flags...]   Prints the work dir; read $W/url.
set -euo pipefail
root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
W="$(mktemp -d "$(cd "${TMPDIR:-/tmp}" && pwd -P)/dolmen-verify.XXXXXX")"
CGO_ENABLED=0 go build -C "$root" -trimpath -o "$W/dolmen" ./cmd/dolmen
port="${PORT:-$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')}"
url="http://127.0.0.1:$port"
env -u DOLMEN_DATA -u DOLMEN_ADDR -u DOLMEN_AUTH -u DOLMEN_ENGINE DOLMEN_EMBED_PROVIDER=none \
  "$W/dolmen" -addr "127.0.0.1:$port" -data "$W/data" "$@" >"$W/server.log" 2>&1 &
echo $! >"$W/pid"
echo "$url" >"$W/url"
for _ in $(seq 1 50); do
  if curl -fsS "$url/healthz" >/dev/null 2>&1; then echo "$W"; exit 0; fi
  sleep 0.2
done
echo "dolmen did not become healthy; log: $W/server.log" >&2
cat "$W/server.log" >&2
exit 1

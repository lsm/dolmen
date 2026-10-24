#!/usr/bin/env bash
# Drive `dolmen mcp` over stdio: initialize, tools/list, and create_table,
# insert and read_rows through tools/call, against a throwaway data dir.
# Requests are answered concurrently, so each waits for its reply first.
# Usage: mcp-stdio.sh <work dir from serve.sh>   (uses $W/dolmen, not the server)
set -euo pipefail
W="$1"
rm -f "$W/mcp.in" "$W/mcp.out"
mkfifo "$W/mcp.in" "$W/mcp.out"
. "$(dirname "$0")/lib.sh"
(scrub_env; DOLMEN_EMBED_PROVIDER=none exec "$W/dolmen" mcp -data "$W/mcp-data") <"$W/mcp.in" >"$W/mcp.out" 2>"$W/mcp.log" &
pid=$!
exec 3>"$W/mcp.in" 4<"$W/mcp.out"
fail=0
rpc() {
  local id="$1" body="$2" want="$3" name="$4" line
  printf '%s\n' "$body" >&3
  [ "$id" = - ] && return 0
  while IFS= read -r -t 30 line <&4; do
    case "$line" in *"\"id\":$id,"*|*"\"id\":$id}"*) break ;; esac
  done
  if printf '%s' "$line" | grep -qF -- "$want"; then echo "PASS $name"; else echo "FAIL $name: want $want"; echo "     got: ${line:0:600}"; fail=1; fi
}
rpc 1 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"verify","version":"0"}}}' '"protocolVersion":"2025-06-18"' initialize
rpc - '{"jsonrpc":"2.0","method":"notifications/initialized"}' '' ''
rpc 2 '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' '"name":"search_vector"' tools/list
rpc 3 '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_table","arguments":{"namespace":"verify","table":"notes","fields":[{"name":"body","type":"text","fulltext":true}]}}}' '"version":1' tools/call-create_table
rpc 4 '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"insert","arguments":{"namespace":"verify","table":"notes","records":[{"body":"hello from mcp"}]}}}' '"inserted":1' tools/call-insert
rpc 5 '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"read_rows","arguments":{"namespace":"verify","table":"notes"}}}' '"hello from mcp"' tools/call-read_rows
rpc 6 '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"read_rows","arguments":{"namespace":"verify","table":"missing"}}}' '"isError":true' tools/call-teaching-error
exec 3>&-
wait "$pid" || true
exit $fail

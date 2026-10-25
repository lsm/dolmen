#!/usr/bin/env bash
# Drive `dolmen mcp` over stdio: initialize, tools/list, and create_table,
# insert, import_records and read_rows through tools/call, against a throwaway data dir.
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
rpc 20 '{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"import_records","arguments":{"namespace":"verify","table":"notes","idempotency_key":"snap/notes/1","records":[{"id":9007199254740993,"created_at":"2026-01-02T03:04:05.123Z","body":"imported over mcp"}]}}}' '"ids":[9007199254740993],"inserted":1' tools/call-import_records
rpc 6 '{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"read_rows","arguments":{"namespace":"verify","table":"missing"}}}' '"isError":true' tools/call-teaching-error
rpc 7 '{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"batch","arguments":{"namespace":"verify","writes":[{"kind":"update","table":"notes","filter":"body = ?","args":["hello from mcp"],"set":{"body":"claimed"},"expected_rows":1}]}}}' '"updated":1' tools/call-batch-expected_rows
rpc 8 '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"batch","arguments":{"namespace":"verify","writes":[{"kind":"update","table":"notes","filter":"body = ?","args":["hello from mcp"],"set":{"body":"claimed"},"expected_rows":1}]}}}' 'expected_rows mismatch' tools/call-batch-expected_rows-stale
rpc 9 '{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"relate","arguments":{"namespace":"verify","table":"notes","source":1,"target":1,"type":"k"}}}' 'not a relation table' tools/call-relate-teaching-error
rpc 30 '{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"create_table","arguments":{"namespace":"verify","table":"nodes","accept_references":true,"fields":[{"name":"label","type":"string"}]}}}' '"accept_references":true' tools/call-create-nodes
rpc 31 '{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"create_table","arguments":{"namespace":"verify","table":"links","fields":[{"name":"src","type":"number","required":true},{"name":"dst","type":"number","required":true},{"name":"kind","type":"string","required":true}],"constraints":[{"name":"src_ref","kind":"foreign_key","field":"src","references":{"table":"nodes","field":"id"}},{"name":"dst_ref","kind":"foreign_key","field":"dst","references":{"table":"nodes","field":"id"}}],"relation":{"source":"src","target":"dst","type":"kind"}}}}' '"name":"links"' tools/call-declare-relation
rpc 32 '{"jsonrpc":"2.0","id":32,"method":"tools/call","params":{"name":"insert","arguments":{"namespace":"verify","table":"nodes","records":[{"label":"first"},{"label":"second"}]}}}' '"inserted":2' tools/call-insert-nodes
rpc 33 '{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"relate","arguments":{"namespace":"verify","table":"links","source":1,"target":2,"type":"knows"}}}' '"id":1' tools/call-relate
rpc 34 '{"jsonrpc":"2.0","id":34,"method":"tools/call","params":{"name":"neighbors","arguments":{"namespace":"verify","node":{"table":"nodes","id":1}}}}' '"stop_reason":"complete"' tools/call-neighbors
rpc 35 '{"jsonrpc":"2.0","id":35,"method":"tools/call","params":{"name":"neighbors","arguments":{"namespace":"verify","node":{"table":"nodes","id":1},"max_edges":1}}}' '"table":"links"' tools/call-neighbors-reference
exec 3>&-
wait "$pid" || true
exit $fail

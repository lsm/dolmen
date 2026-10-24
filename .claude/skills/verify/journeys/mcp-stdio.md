# MCP over stdio

For changes to `internal/mcp`, tool annotations, tool descriptions or any
operation an agent calls through `dolmen mcp`.

```bash
S=.claude/skills/verify/scripts
W=$($S/serve.sh)
$S/mcp-stdio.sh "$W"
$S/stop.sh "$W"
```

It speaks JSON-RPC to `$W/dolmen mcp -data $W/mcp-data`: `initialize`,
`tools/list`, then `create_table`, `insert`, `read_rows`, a missing-table
error and a `batch` claim with `expected_rows` plus its stale retry through `tools/call`. Expect every line `PASS`: results in
`structuredContent` with empty `content`, errors with `isError: true`.

The server answers stdio requests concurrently, so a client must wait for a
reply before sending a request that depends on it; the script does.
MCP over HTTP is the same dispatcher at `$(cat $W/url)/mcp`.

# Semantic graph search (#739)

Read shared SKILL.md. Use synthetic data and no embedding downloads:

```bash
S=.claude/skills/verify/scripts
W=$(env -u DOLMEN_ADMIN_KEY $S/serve.sh)
trap '$S/stop.sh "$W"' EXIT
$S/graph-search.py "$W" "$W/graph-search.json"
CGO_ENABLED=0 go run ./.claude/skills/verify/scripts/graph-search-go
```

Expect three `PASS compiled` lines for HTTP, MCP over HTTP and MCP stdio,
and one `PASS Go SearchGraph` line. The wire journey checks cross-table tie
ordering, separate cosine seed scores, property predicates before ranking,
relation-type filtering, adjacency expansion, node/hop stops and empty search.
Save the JSON and logs before stop.sh removes W. Run test-suites.md as well.

`tables` supplies 1–100 seed tables; `top_k` defaults to 10 and caps at 200.
Use `text` with a configured embedding provider or `vector` for caller-managed
spaces. `filter`/`args` apply to seed properties, not expanded endpoints;
`expansion` accepts subgraph direction, relation_types and bounds. Every seed,
edge and endpoint is scoped in one SQLite snapshot. Scores describe only seeds;
top_k selects seeds and is not a traversal truncation. Unscorable stored vectors
are counted separately. The elapsed budget covers seed scoring and traversal.

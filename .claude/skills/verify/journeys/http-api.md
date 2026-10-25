# HTTP /v1 API

For changes to an operation, `internal/api`, `internal/store`, `internal/value`,
`internal/schema`, envelopes or error messages.

```bash
S=.claude/skills/verify/scripts
W=$($S/serve.sh)
$S/http-smoke.sh "$W"
$S/stop.sh "$W"
```

The smoke covers constraint migrations (duplicate conflict, `add_constraint`, enforcement,
`set_accept_references`, refused combined constraint changes, `drop_constraint` and the next write),
`create_table`, `insert`, `read_rows`, `search_fulltext`,
`search_vector` (caller-supplied vector, no model), `query`, a typed-coercion
error (`invalid_request`), a `batch` compare-and-set with `expected_rows` (the claim, the
stale retry's conflict and its rollback, a refused `null`), `import_records` (a source id
above 2^53, its `created_at` read back, replay, a differing-content `conflict`, and the next
ordinary id above it), `relate` refusing a table that is not a relation table, declaring a relation table (`accept_references`, two `foreign_key` constraints and `relation` on `create_table`), a dangling edge refused as a conflict naming its constraint, then `relate` end to end: an edge, its idempotent replay, a missing endpoint refused, and deleting a referenced row refused, and `/v1/openapi.json`. Expect every line `PASS` and
exit 0.

Then exercise the operation you changed with `call.sh` and read the full
envelope: `{"ok":true,"data":...}` on success, `{"ok":false,"error":{"code",
"message",...}}` on failure. For an error change, check the message names the
remediation and contains no file path. For a schema change, check
`/v1/openapi.json` advertises the new field. Use `serve.sh -auth on` (with
`DOLMEN_ADMIN_KEY` exported to a throwaway value, e.g. `openssl rand -hex 24`)
for auth-gated behavior: unauthenticated calls answer `unauthorized`, and
`curl` with `-H "Authorization: Bearer $DOLMEN_ADMIN_KEY"` reaches the op
(`call.sh` sends no credential).

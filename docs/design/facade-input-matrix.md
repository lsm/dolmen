# Facade input-validation matrix

Status: pinned decisions transcribed from the [#279](https://github.com/lsm/dolmen/issues/279)
parity arc (PRs #301–#309). Documentation-only: every ruling below is already landed in code;
most are pinned by a named test, and directions with no repo pin are marked as such inline.
This file introduces no new behavior. Where surfaces diverge, the divergence is
by-design and recorded per-surface.

## The three surfaces

The two wire transports share one dispatch table (`internal/api/ops.go`); the public Go façade
bypasses it and calls the engine directly (root `table.go`/`read.go`/`write.go` invoke `s.eng`).
One engine serves all three surfaces:

| Surface | Input gate | Name invalid after normalization |
| --- | --- | --- |
| MCP tools | `InputSchema` advertised on `tools/list`: `existingTableProp` grammar, `maxItems` bounds | Grammar is the tool's schema contract |
| `/v1` HTTP | Struct decode (`decode`/`decodeData`, `internal/api/server.go`): `UseNumber` + `DisallowUnknownFields`, no name grammar | Existing-table ops → engine lookup → 404 `not_found`; `create_table` → 400 `invalid_request` |
| Public Go façade | Curated pre-checks (`validTableName`, root `read.go:22`) before `EnsureNamespace`, on guarded methods only | `invalid_request` on reads/search and describe/drop; writes classify via the engine |

`existingTableProp` (`internal/api/server.go:254`) pins the grammar `^[a-z][a-z0-9_]{0,63}$`,
banning `__fts` anywhere in the name and the `sqlite_` prefix; the façade mirrors it in
`validTableName` (`strings.Contains` for `__fts`, `strings.HasPrefix` for `sqlite_`, root
`read.go:23`) and applies it on reads/search (#304) and describe/drop (#309).

Classification throughout this matrix is of the **post-normalization** name: every surface runs
`ops.NormalizeTable` — trim plus lowercase (`internal/ops/normalize.go:21`) — before lookup or
validation; the façade's guarded methods validate the normalized name (root `table.go:77`), and
the wire normalizes in its handlers. A noncanonical spelling such as `" Notes "` therefore
resolves to the existing `notes` table on every surface — code-verified (every handler passes
through `ops.NormalizeTable`); the wire's "table names normalize too" subtest
(`internal/conformance/limits_test.go`) pins creation-side normalization only — it sends
`" Docs "` to `create_table` and then calls `describe_table` with the canonical `docs` — and no
façade-side test pins normalization at all. The `InputSchema` pattern sees the raw token, so a
schema-conforming client on either wire transport rejects a spelling the server runtime
accepts. The table above classifies names that remain invalid after normalization.

The #309 premise correction: the grammar lives in the shared `InputSchema`, advertised on both
wire transports — MCP `tools/list` and `/v1/openapi.json`, whose request bodies are built from
the same `OpDef.InputSchema` (`internal/api/openapi.go`, pinned identical by
`TestMCPInputSchemasMatchOpenAPIRequestSchemas`) — but neither wire runtime re-validates it.
Probing live `/v1` showed the wire classifying malformed names `not_found` on `describe_table`
and `search_fulltext` alike, because the wire decodes typed request structs and applies no
grammar gate. The façade instead rejects impossible names early with `invalid_request` — the
curated-stricter reading that keeps #304's merged decision coherent. The pre-check covers
reads/search and describe/drop only: the write methods (`Insert`, `Update`, `Delete`,
`UpsertByKey` in root `write.go`) normalize the name and hand it to the engine, where a
malformed name matches no table and classifies `not_found` — the same answer the wire gives.
`CreateTable` (root `table.go`) carries no pre-check either, but its engine path enforces the
grammar itself (`schema.ValidateTableName`, `internal/store/store.go:495`), so a malformed name
on create classifies `invalid_request` on both surfaces; the wire's 404 is specifically the
existing-table resolution path.
The divergence is pinned
by-design in `TestEmbeddedParityErrorTaxonomy` (`internal/conformance/embedded_parity_test.go`):
the same malformed name yields wire `not_found` and façade `invalid_request`, each surface's
assert naming the divergence. Wire-side tightening (400 on `/v1`) is a contract change deferred
to a ruling; this doc records the status quo.

## Input limits and where each is enforced

| Limit | Value | MCP schema | Engine runtime | Façade |
| --- | --- | --- | --- | --- |
| Fields per table | `store.MaxFieldsPerTable` = 100 | `create_table` `fields.maxItems` | `CreateTable` (`internal/store/store.go:498`) | Same bound via engine |
| Records per insert | `store.MaxRecordsPerInsert` = 1000 | `insert` `records.maxItems` | `Insert` (`internal/store/insert.go:45`) | Same bound via engine |
| Records per upsert | `store.MaxRecordsPerInsert` = 1000 | `upsert_by_key` `records.maxItems` | `UpsertByKey` (`internal/store/upsert_key.go:19`) | Same bound via engine |
| Delete limit, lower range | wire runtime rejects < 1 with 400 (`parseOptPosInt`, `internal/api/ops.go:1923`); schema advertises `minimum: 1` | `delete` `limit` | — | negatives rejected (root `write.go:145`); explicit 0 = default threshold — an at-zero divergence with the wire (code-verified) |
| Delete limit, upper range | none on any surface | none | none | none |

The advertised `maxItems` bounds and the engine's runtime bounds are the same constants, so wire
and façade agree by construction even where only the engine checks.

## #309 cousin dispositions

The family audit (#309) dispositioned every unaudited cousin line from the parity arc:

| Cousin | Audit result | Disposition |
| --- | --- | --- |
| #301 fields count | engine `CreateTable` enforces `MaxFieldsPerTable`; same bound both surfaces | Cleared + pinned (`TestCreateTableRejectsTooManyFields`, root `familyaudit_test.go`). Order note: façade `EnsureNamespace` runs before engine rejection (namespace side effect on a rejected create) — same as wire handler-level rejections, documented |
| #301 table-name grammar on describe/drop | `/v1` does not enforce the MCP grammar (404 `not_found`); the façade classified `not_found` too | Fixed per the curated-stricter reading + pinned cross-surface with the by-design flag (above) |
| #302 records count | engine `Insert`/`UpsertByKey` enforce `MaxRecordsPerInsert`; same bound both surfaces | Cleared + pinned (`TestInsertRejectsTooManyRecords`, both ops) |
| #302 delete-limit range | wire schema has `minimum: 1`, no maximum; the wire handler also rejects < 1 at runtime (`parseOptPosInt`), so explicit 0 diverges — wire 400, façade default threshold; façade rejects negatives (`TestDeleteRejectsNegativeLimit`, root `write_test.go`), no upper bound either | Cleared — no upper-range divergence exists; parity by construction; the at-zero divergence is code-verified, unpinned |
| #302 record-data floats | NaN stored silently and read back as NULL; ±Inf poisoned the row (every later read errored) | Fixed (`finiteNumber` guard) + pinned; details in [numeric-fidelity-matrix.md](numeric-fidelity-matrix.md) |

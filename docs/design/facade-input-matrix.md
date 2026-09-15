# Facade input-validation matrix

Status: pinned decisions transcribed from the [#279](https://github.com/lsm/dolmen/issues/279)
parity arc (PRs #301–#309). Documentation-only: every ruling below is already landed in code and
pinned by a test; this file introduces no new behavior. Where surfaces diverge, the divergence is
by-design and recorded per-surface.

## The three surfaces

One dispatch table (`internal/api/ops.go`) and one engine serve three surfaces:

| Surface | Input gate | Malformed table name |
| --- | --- | --- |
| MCP tools | `InputSchema` advertised on `tools/list`: `existingTableProp` grammar, `maxItems` bounds | Grammar is the tool's schema contract |
| `/v1` HTTP | Struct decode (`decode`/`decodeData`, `internal/api/server.go`): `UseNumber` + `DisallowUnknownFields`, no name grammar | Flows to the engine → 404 `not_found` |
| Public Go façade | Curated pre-checks (`validTableName`, root `read.go:22`) before `EnsureNamespace` | `invalid_request` — uniformly stricter |

`existingTableProp` (`internal/api/server.go:254`) pins the grammar `^[a-z][a-z0-9_]{0,63}$`,
excluding `__fts` and `sqlite_` prefixes; the façade mirrors it in
`validTableName` and applies it on reads/search (#304) and describe/drop (#309).

The #309 premise correction: the grammar is expressed on the MCP tool schema only. Probing live
`/v1` showed the wire classifying malformed names `not_found` on `describe_table` and
`search_fulltext` alike, because the wire decodes typed request structs and applies no grammar
gate. The façade instead rejects impossible names early with `invalid_request` — the
curated-stricter reading that keeps #304's merged decision coherent. The divergence is pinned
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
| Delete limit, lower range | `minimum: 1` on the wire | `delete` `limit` | — | negatives rejected (root `write.go:145`) |
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
| #302 delete-limit range | wire schema has `minimum: 1`, no maximum; façade rejects negatives (`TestDeleteRejectsNegativeLimit`, root `write_test.go`), no upper bound either | Cleared — no upper-range divergence exists; parity by construction |
| #302 record-data floats | NaN stored silently and read back as NULL; ±Inf poisoned the row (every later read errored) | Fixed (`finiteNumber` guard) + pinned; details in [numeric-fidelity-matrix.md](numeric-fidelity-matrix.md) |

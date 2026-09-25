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
| `/v1` HTTP | Struct decode (`decode`/`decodeData`, `internal/api/server.go`): `UseNumber` + `DisallowUnknownFields`, no name grammar | Existing-table ops → engine lookup; a name that breaks the grammar → 400 `invalid_request`, a well-formed name matching no table → 404 `not_found` |
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

This split is deliberate (#43). The advertised schemas are the strict contract; the server is
the lenient reader, and it reads alike on every transport, because `/v1` and MCP share one
dispatch table: it trims and lowercases names, and it clamps a search `limit` (≤ 0 selects the
default of 10, anything above 200 becomes 200) rather than refusing it. A client that validates
against the schemas never sends what the server would normalize; one that does not gets the same
answer on either transport. `TestTransportParityOnInputTheSchemasWouldRefuse` pins that
agreement case by case. The one bound the server enforces before decoding is the query vector's
length (`maxItems` = 4096 on `search_vector.vector`), because an unbounded array is expanded in
memory before any dimension check can reject it; the façade applies the same bound through
`ops.PrepareVectorQuery`.

The #309 premise correction: the grammar lives in the shared `InputSchema`, advertised on both
wire transports — MCP `tools/list` and `/v1/openapi.json`, whose request bodies are built from
the same `OpDef.InputSchema` (`internal/api/openapi.go`, pinned identical by
`TestMCPInputSchemasMatchOpenAPIRequestSchemas`) — but neither wire runtime re-validates it.
The wire decodes typed request structs and applies no grammar gate of its own. The engine
does, when resolving the table: `store.TableNotFound` answers a name that breaks the grammar
with `invalid_request` naming the rule it breaks, and a well-formed name that matches no table
with `not_found` pointing at `list_tables`, on both engines (#325). The façade additionally
rejects impossible names early on reads/search and describe/drop; its write methods
(`Insert`, `Update`, `Delete`, `UpsertByKey` in root `write.go`) hand the name to the engine
and get the same `invalid_request` the wire does.
`CreateTable` (root `table.go`) carries no pre-check either, but its engine path enforces the
grammar itself (`schema.ValidateTableName`, `internal/store/store.go:495`), so a malformed name
on create classifies `invalid_request` on both surfaces too.
The wire used to answer a malformed name on the existing-table path with 404 `not_found`, a
recorded divergence from the façade. That was ruled a contract change worth making (#325): a
hyphen in a table name is a common first mistake, and `not_found` sent callers looking for a
table they never created. Both surfaces now agree, pinned in `TestEmbeddedParityErrorTaxonomy`
(`internal/conformance/embedded_parity_test.go`).

## Input limits and where each is enforced

| Limit | Value | MCP schema | Engine runtime | Façade |
| --- | --- | --- | --- | --- |
| Fields per table | `store.MaxFieldsPerTable` = 100 | `create_table` `fields.maxItems` | `CreateTable` (`internal/store/store.go:498`) | Same bound via engine |
| Query vector length | `schema.MaxVectorDim` = 4096 | `search_vector` `vector.maxItems` | Streaming count before the body is decoded (`internal/api/ops.go`), then `ops.PrepareVectorQuery` | Same bound via `ops.PrepareVectorQuery` |
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
| #301 fields count | engine `CreateTable` enforces `MaxFieldsPerTable`; same bound both surfaces | Cleared + pinned (`TestCreateTableRejectsTooManyFields`, root `familyaudit_test.go`). Order note: on both surfaces the namespace is ensured before the engine rejects, so a rejected create may leave an empty namespace behind — documented, and unchanged by #39, which removed the ensure from reads only |
| #301 table-name grammar on describe/drop | `/v1` did not enforce the MCP grammar (404 `not_found`); the façade classified `not_found` too | Fixed: both surfaces answer `invalid_request` (#325), pinned cross-surface (above) |
| #302 records count | engine `Insert`/`UpsertByKey` enforce `MaxRecordsPerInsert`; same bound both surfaces | Cleared + pinned (`TestInsertRejectsTooManyRecords`, both ops) |
| #302 delete-limit range | wire schema has `minimum: 1`, no maximum; the wire handler also rejects < 1 at runtime (`parseOptPosInt`), so explicit 0 diverges — wire 400, façade default threshold; façade rejects negatives (`TestDeleteRejectsNegativeLimit`, root `write_test.go`), no upper bound either | Cleared — no upper-range divergence exists; parity by construction; the at-zero divergence is code-verified, unpinned |
| #302 record-data floats | NaN stored silently and read back as NULL; ±Inf poisoned the row (every later read errored) | Fixed (`finiteNumber` guard) + pinned; details in [numeric-fidelity-matrix.md](numeric-fidelity-matrix.md) |

## Divergences recorded from the pre-v0.3.0 audit (#328)

Each row was code-verified when recorded. The façade is the stricter surface in every row but the
last. The two transports differ only in the decode rows, where MCP screens the JSON-RPC envelope
before the shared dispatch table sees the arguments.

| Input | Façade | `/v1` | MCP |
| --- | --- | --- | --- |
| Search `Limit` outside 1–200 | `invalid_request` (root `search.go`) | clamped, as #43 records | clamped |
| Search `Offset` above 1,000,000,000 | `invalid_request` | accepted; the bound is schema-only | accepted |
| Blank but non-empty search `Filter` | `invalid_request` | treated as no filter | treated as no filter |
| Empty `IdempotencyKey` | treated as no key | `invalid_request` | `invalid_request` |
| Empty query vector | `vector must have at least one element` | `pass either text or vector` | same as `/v1` |
| Body that is not a JSON object | — | `400 invalid_request`, naming the object requirement | JSON-RPC `-32602`, `tools/call arguments must be an object` |
| `null` body | — | treated as `{}`, as v0.2.0 did | JSON-RPC `-32602` |
| Empty or whitespace-only body | — | treated as `{}` | absent `arguments` are treated as `{}` |
| Secret `reveal` (#467) | always allowed: the façade has no auth | allowed with `-auth off`; with `-auth on` needs the `reveal` verb (`forbidden` otherwise, and always for the bootstrap admin key), within the op's row scope | same as `/v1` |

## Namespace creation on reads (#39)

Parity by construction, no divergence: **no read creates a namespace on either surface.** The write
ops — `create_table`, `insert`, `update`, `upsert`, `upsert_by_key`, `delete`, `migrate` — still
create implicitly on first use, on both surfaces.

| Surface | Reads that answer `not_found` for a missing namespace, creating nothing |
| --- | --- |
| MCP + `/v1` | `list_tables`, `describe_table`, `read_rows`, `query`, `search_fulltext`, `search_vector`, `changes_since`, `wait_for`, `list_migrations`, `subscribe`, `drop_table` |
| Go façade | `ListTables`, `DescribeTable`, `GetRows`, `Query`, `SearchFulltext`, `SearchVector` (both the text and raw-vector branches), `DropTable` |

Before #39 both surfaces resolved reads through `ops.EnsureNamespace`, so discovery created the
namespace file, its registry, and its connections — a mistyped name left `<ns>.db`, `-wal` and
`-shm` on disk. The engine's own open path already returns `ErrNotFound` for a missing file without
creating it, which is how `wait_for` always answered `not_found`; the reads simply never reached it.

Pinned cross-surface in the same PR: `TestReadOpsNeverCreateNamespaceFiles` and
`TestWriteOpsStillCreateNamespacesImplicitly` (`internal/conformance/namespace_test.go`, over `/v1`
and MCP), and `TestEmbeddedParityReadsNeverCreateNamespaces` /
`TestEmbeddedParityWritesStillCreateNamespaces` (`internal/conformance/embedded_parity_test.go`).
Each asserts the data directory is empty afterwards, so a future reintroduction of an ensure on a
read fails on the surface that regressed.

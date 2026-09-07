# Implementation plan — the #159 epic in 49 S-sized slices

This document is the **execution plan** for the epic specified in
`docs/design/identity-and-engines.md` (the design authority — "the spec"). Every decision the spec
pins is settled; this plan says **in what order, in what size, and in which files** the work lands.
If this plan and the spec ever disagree, the spec wins and this document is edited first.

Grounding: every slice below was written against the actual v0.2.0 tree (all production files read;
506 test functions inventoried; the conformance suite read in full). File references are real paths
at the current `main`.

## Slice conventions

- **Every slice is S-sized**: one focused PR, one sitting's work, lands green (`go vet`, `go test
  ./...` race-enabled via `make test`, `govulncheck`), conformance-visible where the slice has
  contract surface.
- **Each slice becomes one thin GitHub issue**, generated from its entry here — title, spec §ref,
  file list, acceptance. No paraphrase of the spec (the drift lesson of the closed stream issues).
- **Additive-only**: nothing v0.2.0 accepts may change meaning (spec §8.1). Response-shape additions
  are permitted; removals and re-interpretations are not.
- **Docs ride in the slice's DoD**: a slice that changes agent-visible behavior updates
  `skill/dolmen.md` / `skill/dolmen-admin.md` / `README.md` lines in the same PR. Issue 11 is the
  final polish pass, not the only docs work.
- Dependency notation: **Dep** lists the slices that must merge first. Everything else runs in
  parallel.

## Waves and lanes

```
Lane 0        1 (harness modes)

Lane A        2a → 2b → 2c ─┬─ 3a → 3b → 3c ─┬─────────────→ 8a … 8g → 9a … 9j
(storage +                   └─ 4a → 4b → 4c → 4d ─ 5b → 5c → 5d ─ 6a → 6b → 6c
 realtime)                    └─ 5a (any time after 2c)

Lane B        7a … 7e ─┬─ 8c … 8g (grants, with Lane A's 3b/4a)
(auth,                     └─ 10a … 10g (OIDC + keys, needs 8a/8d)
 native-first)

Closing       11
```

Filing cadence (drift control): Lane 0 + Lane A issues are filed now; Lane B issues file when
Lane A is moving. **Deliberately not filed — demander-gated (spec D20/D25):** the Postgres adapter,
the lakehouse adapter, webhooks, export/import, job-queue/claim semantics.

---

## Lane 0 — enabler

### 1. Conformance harness: auth-mode parameterization
**Spec:** §8.2–8.3 · **Dep:** — · **Wave:** files first

Goal: the harness can boot a server in a mode other than today's implicit `auth: off`, and tests
can make identity-carrying calls, before any auth code exists to use it.

Changes:
- `newHarnessMode(t, mode)` boot path: a mode struct (initially only `off`) holding the future
  auth server options, so slice 7b extends the struct instead of reworking the harness.
- Identity-carrying call helpers (`httpCallAs(identity, op, body)`, `mcpCallAs`) that inject
  headers/bearers when the mode asks for them; under `off` they assert the server ignores identity.
- Assert-on-boot helper: `describe_server` response byte-shape unchanged in `off` mode.

Files: `internal/conformance/harness_test.go` (extend), new
`internal/conformance/harness_mode_test.go`.

Acceptance: full existing suite green through the new boot path; the mode struct compiles with
placeholder fields commented as "activated by 7b/10g".

---

## Lane A — storage + realtime

### 2a. `store.Engine` interface definition
**Spec:** §6.1–6.3 · **Dep:** — 

Goal: the seam's contract file exists — interface, guard types, result types — with zero wiring.
Signatures are spec-pinned **now** (guards included, zero-value = no-op) so no later slice ever
re-signatures the interface.

Changes:
- `Engine` interface with the §6.2 operation set: namespace lifecycle
  (`NamespaceState`, `ListNamespaces`, `CreateNamespace`, `DropNamespace`), table DDL/registry
  (`TableState`, `ListTables`, `CreateTable`, `DescribeTable`, `DropTable`), migrations
  (`PlanMigration`, `Migrate`, `ListMigrations`), row CRUD (`Insert`, `UpsertByKey`, `Upsert`,
  `GetRows`, `Update`, `Delete`), change log (`ChangesSince`, `Listen`), filtered reads
  (`Query`), search (`SearchFulltext`, `SearchVector`), `Capabilities`, `Close`.
- Types: `AuthBinding` (per §6.2 comment block), `Incarnation{NsGen, Table, Version, DropGen}`,
  `RowScope{Owner, Empty}`, `WriteOpts{Owner, IdempotencyKey, TableWideRead}`,
  `DeleteOpts`/`DeleteResult`, `InsertResult`/`UpdateResult` (UpdateResult carries the change
  cursor range, not a bare count), `Page`, `Cursor`, `ChangeRecord`, `ChangeReplay`,
  `EngineCapabilities`.
- Doc comments copied tight from §6.2 (the global rules: never create implicitly, atomic
  incarnation verification, empty-bindings meaning).

Files: `internal/store/engine.go` (new).

Acceptance: package compiles; no production behavior touched; doc comments name the spec sections.

### 2b. `*Store` satisfies `Engine`
**Spec:** §6.2 · **Dep:** 2a

Goal: the concrete store implements the interface, mechanically. Guard parameters arrive and are
ignored (recorded as TODO-with-slice-number); behavior identical.

Changes:
- Store methods gain the interface parameters (`nsGen`/`inc`/`scope`/`scopeIncarnation`/
  `bindings`/`page` where the interface has them). Signatures that change shape
  (`CreateNamespace`/`DropNamespace` gain `ctx` + guard args; `ListNamespaces` gains `ctx`,
  `prefix`, `bindings`; `Query` gains `nsGen`, `Page`).
- **Contained test churn:** Go cannot overload methods, so `*Store` cannot carry both the old
  arities and the `Engine` signatures under one name — the old arities move to a separate
  `legacy` adapter (`type legacyStore struct{ *Store }`, package helper `legacy(s)`) whose
  methods delegate with zero values; store-test suites switch their constructor once (one line
  per setup, not per call site), so the ~300 direct call sites keep compiling unchanged. The
  adapter is deleted in 7e/9i when the api layer supplies real values.
- The ~19 **production** call sites in `internal/api/ops.go` are adapted in the same slice —
  mechanical zero-value arguments (guards, nil scopes): changing `*Store`'s signatures while
  wrapping only the store tests would leave `internal/api` uncompilable, and 2b must land
  green. 2c then switches those calls from the concrete type to the `Engine` variable.
- Two concrete-only helpers fold into the interface's paths as part of that adaptation:
  `InsertIdempotent` call sites become `Insert` with `WriteOpts.IdempotencyKey`, and
  `ValidateVectorSearch` call sites become the `TableState`-snapshot validation §6.2 specifies
  (vectorize-field presence and identity pinned before the provider is called) — neither
  method exists on `Engine`, so leaving them would break 2c's swap.

Files: `internal/store/store.go`, `lifecycle.go`, `insert.go`, `update.go`, `upsert_key.go`,
`search.go`, `vector.go`, `query.go`, `migrate.go`, `internal/api/ops.go` (call-site
adaptation); new `engine_compat_test.go`
(`var _ Engine = (*Store)(nil)` plus a compile-drift guard).

Acceptance: compile-time proof; full test suite green unchanged.

### 2c. `api.Server` holds an `Engine`
**Spec:** §6 · **Dep:** 2b

Goal: the last concrete dependency above the seam is gone. `api` no longer imports the concrete
type except where it constructs nothing — it programs against `Engine`.

Changes:
- `Server.st *store.Store` → `eng store.Engine` (`internal/api/server.go:26`); `New()` signature
  keeps accepting what it gets (a `*Store` satisfies `Engine`; `main.go` and the harness unchanged).
- All 19 op funcs in `internal/api/ops.go` call through the interface (their arities were
  adapted to zero-value guards in 2b; this slice switches them from the concrete type to the
  `Engine` variable), still passing zero-value guards and nil scopes (auth is off everywhere
  until Lane B).
- `embedder()` helper unchanged.

Files: `internal/api/server.go`, `internal/api/ops.go` (mechanical call-site updates).

Acceptance: entire conformance suite green — this slice's diff is the proof of the spec's
"zero contract change" claim.

### 3a. Namespace path grammar + nested file layout
**Spec:** §5.1–5.2 · **Dep:** 2c

Goal: namespaces become depth-≤3 slash paths; adapter #1 maps `a/b/c` to `<data>/a/b/c.db`.
Depth-1 behavior byte-identical (§5.2: no migration, no behavior change).

Changes:
- `nsRe` single-segment regex (store.go:30) becomes a segment regex + `validateNSPath`
  (1–3 segments, no empty/leading/trailing/double slashes).
- `nsPath` joins nested (`lifecycle.go:217`); `ns()` creates parent directories on first use of a
  child; the `nss` cache keys by full path. A namespace and its subtree coexist (`a.db` beside
  `a/`).
- Existing call sites validate via `validateNSPath`; store tests for path grammar, layout, and
  coexistence.

Files: `internal/store/store.go`, `internal/store/lifecycle.go`, store tests.

Acceptance: depth-1 conformance byte-identical; new store tests pin `a/b/c` layout and `a` +
`a/b` coexistence.

### 3b. Recursive listing + prefix filter + leaf-only drops
**Spec:** §5.3–5.4 · **Dep:** 3a

Goal: `ListNamespaces` walks the tree; the op gains optional `prefix`; drops refuse non-leaves.

Changes:
- `ListNamespaces` recursive walk (replaces flat `ReadDir`, `lifecycle.go:18-36`); optional prefix
  (valid path) filters to the recursive subtree; lexicographic full-path order (depth-1-only
  stores sort identically — §5.3).
- `DropNamespace` rejects a namespace with descendants (`invalid_request` naming the descendant
  count) before any eviction/deletion (`lifecycle.go:84`).
- `list_namespaces` op accepts optional `prefix` (additive, §8.1).

Files: `internal/store/lifecycle.go`, `internal/api/ops.go` (list_namespaces), tests.

Acceptance: conformance — v0.2.0 `list_namespaces` (no prefix) identical; prefix listing and
leaf-only-drop cases pinned.

### 3c. API surface: path patterns + per-segment normalization
**Spec:** §5.1 · **Dep:** 3b

Goal: every schema surface accepts the path form; request normalization is per-segment.

Changes:
- `nsProp` pattern widened to the path grammar (`server.go:211-217`); `normNS` lowercases/trims
  per segment (`server.go:380`).
- OpenAPI `TableSchema.namespace` pattern widened (`openapi.go:142`); op descriptions mentioning
  single-segment names updated; skills/README namespace guidance lines.

Files: `internal/api/server.go`, `internal/api/openapi.go`, `internal/api/ops.go` descriptions,
`skill/dolmen.md`, `README.md`; conformance.

Acceptance: conformance deep-path scenarios (create under `a/b`, list with prefix, drop child
then parent); schema-additive only.

### 4a. nsGen: namespace creation id
**Spec:** §6.2 (NamespaceState), §3.4 · **Dep:** 3a

Goal: every namespace lifetime has a random 128-bit identity, minted at creation, persisted in the
namespace's own registry.

Changes:
- `_dolmen_meta(key TEXT PRIMARY KEY, value BLOB)` added to `registryDDL` (store.go:116-148);
  `nsGen` minted (crypto/rand, 16 bytes) on first init and read thereafter.
- `NamespaceState` (declared in 2a, implemented on `*Store` here): returns the id or
  `ErrNotFound`; the zero-value guard semantics unchanged.

Files: `internal/store/store.go`, new `internal/store/nsgen.go`; store tests.

Acceptance: nsGen stable across reopen; distinct after drop+recreate; `NamespaceState` not_found
for absent namespaces (no implicit creation in the read).

### 4b. Change log: table + minting in the insert path
**Spec:** §9.3 (records minted in the write transaction) · **Dep:** 4a

Goal: the durable per-namespace log exists; **insert** transactions mint records; write results
carry the cursor range internally. Public response shapes unchanged (notification plumbing only).

Changes:
- `_dolmen_changes(seq INTEGER PRIMARY KEY AUTOINCREMENT, table_name TEXT NOT NULL, row_id
  INTEGER NOT NULL, kind TEXT NOT NULL, owner TEXT, nsgen BLOB NOT NULL, drop_gen INTEGER NOT
  NULL, at TEXT NOT NULL DEFAULT …)` in `registryDDL`; the AUTOINCREMENT sequence *is* the
  per-namespace cursor (monotonic, gap-free by construction).
- `mintChanges(ctx, tx, table, kind, ids, owners)` helper (new `internal/store/changelog.go`):
  assigns contiguous seqs inside the caller's transaction, one row per affected id, recording the
  table's current drop generation and the namespace's nsGen. The owner label is captured
  **per row**: insert branches stamp the caller (`WriteOpts`); update and delete read each
  affected row's own owner from the write's materialized id set — a single caller-value label
  would relabel other users' rows on table-wide updates and deletes (§9.3's label is the row's,
  not the writer's).
- `insertAttempt` (insert.go:127-313) calls it beside the idempotency insert; `InsertResult`
  gains the internal `ChangeRange{First, Last, Count}` — **never a materialized slice** (§6.2).

Files: `internal/store/store.go` (DDL), new `internal/store/changelog.go`,
`internal/store/insert.go`, `internal/store/engine.go` (result types already declared).

Acceptance: after any insert, a direct-SQL fixture sees contiguous `_dolmen_changes` rows in the
same namespace db; rollback leaves zero rows; conformance byte-identical (responses unchanged).

### 4c. Minting in the remaining write paths
**Spec:** §9.3 · **Dep:** 4b

Goal: upsert_by_key, update/upsert, and delete mint records in their transactions.

Changes:
- `upsertKeyAttempt` (upsert_key.go:134-370): one record per record-branch (insert or update),
  minted in-transaction.
- `updateOrUpsert` (update.go:43-280): records for matched rows via the materialized
  `_dolmen_update_ids` set, each carrying that row's own owner read from the materialization
  (owner is immutable — callers cannot set it); `UpdateResult` carries the range.
- `Delete` (search.go:261-345): records for the `_dolmen_delete_ids` set — delete events carry the
  owner stamp from the pre-delete rows (owner is NULL until 9c; the column exists now so no
  registry rebuild later).
- `DropTable` does **not** purge change records (lifetime labels, not deletion — §3.4/D24);
  `DropNamespace` deletes the log with the file, free.

Files: `internal/store/upsert_key.go`, `update.go`, `search.go`, `changelog.go`; store tests.

Acceptance: per-path minting tests incl. bulk update (`1=1`) and confirmed delete; rollback
cleanliness; drop-and-recreate labels distinguishable by drop_gen.

### 4d. Post-commit notification registry
**Spec:** §9.3 (notification after commit; loss harmless) · **Dep:** 4c

Goal: in-process waiters can be woken after commit; the engine declares the capability.

Changes:
- New `internal/store/notify.go`: a mutex-guarded per-namespace listener list;
  `notifyCommitted(ns, table, range)` invoked **after** `tx.Commit()` returns in every write path.
- `Capabilities()` on `*Store`: `vector_execution:"exact"`, `notifications:true`,
  `subscribe:false` (true from 6b), `ann_recall_bound:null`.

Files: `internal/store/notify.go` (new), write paths (one call each),
`internal/store/engine.go` implementation note.

Acceptance: a registered test listener observes ranges after commit only (never before); a panic
in a listener cannot fail the write (recovered, logged).

### 5a. `read_rows` + `capabilities` ops
**Spec:** §2 (verb table rows) · **Dep:** 2c

Goal: two small contract ops land early — the by-id fetch the realtime recovery path needs, and
the engine capability surface.

Changes:
- `GetRows(ctx, ns, table, ids, scope, scopeIncarnation)` on `*Store`: id-addressed fetch reusing
  the `fetchByIDs` projection path (search.go:153); ids capped at 1000 (`invalid_request`
  beyond); missing/invisible ids simply absent; ascending id order.
- `read_rows` op in `Ops` (request `{namespace, table, ids}`, response `{rows, row_count}`) —
  present in both modes, additive under `auth: off`.
- `capabilities` op: serializes `Engine.Capabilities()` verbatim (pinned field names/types per
  §6.2); both modes.
- `toolAnnotations` entries (mcp/server.go:51) and `outputSchemas` (openapi.go:26) for both ops.

Files: `internal/store/engine.go` impl, new `internal/store/getrows.go`,
`internal/api/ops.go`, `internal/mcp/server.go`, `internal/api/openapi.go`; conformance.

Acceptance: conformance — read_rows parity across transports, cap enforcement, absence-not-error
for unknown ids; capabilities shape pinned.

### 5b. Cursor tokens + retention
**Spec:** §9.3 (cursors durable; retention R; token opacity) · **Dep:** 4b

Goal: opaque resume tokens and the retention knob exist at the engine level, before the ops.

Changes:
- `_dolmen_cursor_tokens(token TEXT PRIMARY KEY, position INTEGER NOT NULL, issued_at INTEGER NOT
  NULL, chain_id TEXT NOT NULL, chain_origin INTEGER NOT NULL, feed_table TEXT NOT NULL DEFAULT
  '' — `''` = the unfiltered namespace feed)` in the namespace db — the coordinated-storage
  mapping (§9.3: "the client never sees position-derived bytes"; satisfies the opacity/length
  rules trivially, deployment-wide by living in the durable db; namespace-lifetime binding is
  inherent — the mapping dies with the namespace db, §5.4). The token is bound to its FEED:
  resolve verifies the caller's table selector against the stored `feed_table` — a token
  minted on table A's feed replayed against table B (or an unfiltered feed) is rejected as a
  cross-feed reuse, never honored as a position, which would silently skip B's events.
- Helpers in `changelog.go`: mint (random token), resolve, head position, `begin` boundary
  (oldest with full page-chain headroom: `M ≥ T−R`), page-chain deadline refresh + absolute cap
  `chain_start + 2R`, age-based pruning of records and tokens (`R = 0` disables).
- `-change-retention` / `DOLMEN_CHANGE_RETENTION` (default `168h`, valid `0` or `1h`–`2160h`,
  startup-rejected outside) in `main.go` config (`loadConfig`, env help).

Files: `internal/store/store.go` (DDL), `internal/store/changelog.go`, `main.go`; store tests.

Acceptance: token mint/resolve round-trip survives reopen; pruning bounded by age only (never
count); `begin` boundary math pinned by table-driven tests.

### 5c. `changes_since` op
**Spec:** §9.2–9.3 · **Dep:** 5b, 4c

Goal: the replay op — the foundation layer of realtime, agent-usable today.

Changes:
- `ChangesSince` on `*Store`: paged read in cursor order; table filter selects the table's
  **current lifetime** only (drop_gen/nsgen labels); namespace feed guarded by nsGen.
- Op `changes_since`: request `{namespace, cursor?, table?, begin?, limit?}`; omitted/zero cursor
  = current head (wake-up semantics: fresh subscribers get future events only, and the response
  carries the head cursor); `begin` = the retained-history boundary from 5b; `limit` default 100
  / max 1000; response `{changes:[{cursor, table, row_id, kind}], next_cursor}`. Public records
  expose cursor/table/row_id/kind only — owner never (internal label until 9d makes rows
  visible-set-filtered).
- Beyond-retention = teaching error naming the catch-up path. Tool annotations + OpenAPI.

Files: `internal/store/changelog.go`, `internal/store/engine.go` impl, `internal/api/ops.go`,
`internal/mcp/server.go`, `internal/api/openapi.go`, skills; conformance.

Acceptance: conformance §8.3 item 7 subset — event-on-write, cursor replay after reopen,
gap-free sequences, reconnect catch-up, beyond-retention teaching error.

### 5d. `wait_for` op
**Spec:** §9.2 (layer 2) · **Dep:** 5c, 4d

Goal: the long-poll — the single most agent-visible realtime primitive; fully usable over plain
MCP/HTTP with no special client.

Changes:
- Op `wait_for`: request adds `timeout_ms` (default 30000, valid 0–60000,
  `invalid_request` outside; `0` = immediate conditional poll); response = `changes_since`'s page
  semantics; on timeout an **empty page carrying the unchanged head cursor, never an error**.
- Implementation: check the log; if empty and `timeout_ms > 0`, wait on the 4d registry (channel
  with deadline) with a poll tick fallback (e.g. 250 ms) — the degraded-mode contract holds even
  without a wake; return on first matching commit. Correctness does not rest on the tick: the
  waiter registers with the notification registry BEFORE the final emptiness check, and the
  timeout path re-checks the log before returning an empty page, so a matching commit between
  the initial check and registration is never missed (a sub-tick timeout can never wrongly
  return empty) — the tick is a fallback for lost wakeups, not the race guard.
- Skill guidance (dolmen.md): the poll-replacement pattern with a one-tool-call example.

Files: `internal/store/changelog.go` (WaitFor helper), `internal/api/ops.go`,
`internal/mcp/server.go`, `internal/api/openapi.go`, `skill/dolmen.md`; conformance.

Acceptance: conformance — wake-on-write (insert from a second harness client wakes a blocked
waiter), timeout-empty, `timeout_ms: 0` behavior, cursor-resume chain.

### 6a. `subscribe`: SSE handler, replay half
**Spec:** §9.2 (layer 3) · **Dep:** 5c

Goal: the HTTP-surface stream handler exists — replay then close — but stays UNREGISTERED: the
endpoint's specified behavior is a live stream (6b), and a client discovering a registered
replay-then-terminate route on an intermediate revision could mistake the terminal frame for
end-of-subscription and miss subsequent commits. The route joins the mux in 6b, with live
streaming; here the handler is exercised directly (`httptest` against the handler).

Changes:
- SSE handler (an HTTP-surface capability like `/mcp` — not an `Ops` entry): query params
  `namespace`, `table?`, `cursor?`/`begin?`; content-type `text/event-stream`, immediate flush;
  replay events from 5c in cursor order; then a close frame (6b replaces close with live
  streaming). Teaching errors as SSE error events with the standard envelope inside.
- `wait_for` remains the MCP-surface equivalent (transport parity note, §2).

Files: new `internal/api/sse.go`, `internal/api/server.go` (route registration lands with 6b);
conformance (streaming read via `httptest` + bufio, handler-direct).

Acceptance: handler-level conformance — a subscriber with a cursor receives exactly the missed
events then a terminal frame; bad cursors get the teaching error event; no route registered.

### 6b. `subscribe`: live streaming
**Spec:** §9.3 (atomic register-and-replay) · **Dep:** 6a, 4d

Goal: streams stay open and deliver live commits, with no commit-before-registration gap.

Changes:
- `Listen` implemented on `*Store` per the §6.2 contract: registration fixes the replay boundary
  atomically; `notify` not invoked until the replay drains to the boundary; interim commits
  buffer in a bounded queue; overflow closes the stream with the teaching reconnect recipe.
- The SSE handler drives replay-then-live through `Listen`; client disconnect cancels cleanly.
- `GET /v1/subscribe` joins the api mux — the endpoint goes public exactly when its specified
  live-stream behavior exists (6a's handler stays handler-tested until then) — and
  `Capabilities().subscribe` flips to `true` in the same slice: the portable capability
  surface and the registered route change together, never contradicting each other.

Files: `internal/store/notify.go` (Listen), `internal/store/engine.go` (capabilities),
`internal/api/sse.go`; store + conformance tests.

Acceptance: conformance — a write during replay is neither duplicated nor skipped (exactly-once
across the boundary); slow-drain overflow triggers the reconnect frame; disconnect releases the
listener; the registered route serves replay-then-live; the capabilities op reports
`subscribe: true`.

### 6c. Subscription age bound + resume contract
**Spec:** §1.2 (`-max-subscription-age`), §9.3 · **Dep:** 6b

Goal: source-A identity-refresh backstop and the durable resume contract, pinned.

Changes:
- `-max-subscription-age` / `DOLMEN_MAX_SUBSCRIPTION_AGE` (default `30m`, valid `0` or
  `1s`–`24h`, `0` disables with the documented caveat) in `main.go`; teaching close at the bound
  carrying the resume cursor.
- Resume contract pinned in conformance: disconnect + reconnect-with-cursor equals
  catch-up-then-live (no loss, no duplication).

Files: `main.go`, `internal/api/sse.go`, `internal/store/notify.go`; conformance.

Acceptance: conformance — age-bound close fires with resume token; reconnect-equals-catch-up
holds under interleaved writes.

---

## Lane B — auth, native-first

### 7a. Auth configuration plumbing
**Spec:** §1.2 config table, §1.3 · **Dep:** —

Goal: the flags/env exist and validate at startup; nothing consumes them yet (auth off is the
default and byte-identical).

Changes:
- `-auth` / `DOLMEN_AUTH` (`off`|`on`), `-trusted-proxies` / `DOLMEN_TRUSTED_PROXIES`
  (comma CIDRs, bare IPs allowed; parse errors rejected at startup), `-max-groups` /
  `DOLMEN_MAX_GROUPS` (default 128, range 1–1024), `DOLMEN_ADMIN_KEY` (env-only; regex
  `^[A-Za-z0-9_-]{32,256}$`; a `dlm_`-prefixed value is a startup error, §1.3) — in `loadConfig`
  (`main.go:142-225`) + env help table.
- Config struct fields plumbed to `api.New` via a new option (carried, unused until 7b).

Files: `main.go`, `internal/api/server.go` (option); `main_test.go`.

Acceptance: config validation tests (every malformed value named); auth-off boot identical.

### 7b. Identity middleware: header source + admin key
**Spec:** §1.1–1.3, D1–D5 · **Dep:** 7a, 2c

Goal: under `auth: on`, every `/v1/{op}` and `/mcp` request resolves to exactly one
`(principal, groups)` or fails 401 `unauthorized`; under `auth: off` the middleware is a no-op and
the surface is byte-identical.

Changes:
- New `internal/api/auth.go`: `Identity{Principal, Groups, Source}`;
  `resolveIdentity(r, cfg)` — trusted-proxy check on the **immediate TCP peer** (`RemoteAddr`,
  never XFF); `X-Dolmen-Principal` charset `^[!-~]{1,256}$`; `X-Dolmen-Groups` parse (trim, drop
  empties, dedupe preserving order, each surviving entry validated against §1.1's
  `^[!-~]{1,128}$` — internal spaces, non-ASCII, or over-length entries are malformed identity
  = 401, never silently accepted; cap at `-max-groups` — over-limit = 401); bearer precedence
  over headers with fail-closed (invalid bearer = 401 even behind a valid proxy identity);
  admin-key compare constant-time; `dolmen-admin` reservation (header asserting it = 401);
  unauthenticated paths per §1.2 (`/healthz`, `/version`, `/skills*`, `/v1/openapi.json`).
- `ErrCodeUnauthorized` (401) in `envelope.go`; distinct from 403 `forbidden`.
- Middleware wired in the `/v1/` handler (`server.go:466`), `mcp.ServeHTTP`
  (`mcp/server.go:119`), and the `/v1/subscribe` SSE route (its own mux entry since 6b —
  streams carry identity too); identity into the op-context and the request's log line beside
  `X-Request-Id`.
- Fail-closed rollout: `-auth on` is a **startup error until 8d** — the mode that promises
  deny-by-default never boots without deny-by-default AND its startup invariants
  (source-presence, usable root administrator), so no revision is deployable with
  authenticated-but-permissive dispatch or an administratively dead boot. The middleware and
  its 401 rules are exercised here at unit/handler level; `auth: off` stays byte-identical;
  7d's gateway fixtures (post-8d) cover the end-to-end surface.
- `whoami` op (auth:on-only: absent from dispatch under off — §2 transport parity).

Files: new `internal/api/auth.go`, `internal/api/envelope.go`, `internal/api/server.go`,
`internal/api/ops.go` (whoami), `internal/mcp/server.go`; tests.

Acceptance: unit tests for every 401 rule; auth-off conformance byte-identical; startup warning
line at `main.go:100` updated to reflect mode.

### 7c. Startup checks (pre-grants)
**Spec:** §1.2 · **Dep:** 7a

Goal: fail-fast at boot for decidable misconfigurations.

Changes:
- `auth: on` with no identity source at all (no trusted proxies, no admin key) = startup error.
- Admin-key-only deployments validated per §1.2. (The root-administrator usability check needs
  grants — 8d completes it; the `auth: on` runtime checks are exercised from 8d, when the mode
  first boots.)

Files: `main.go` (run), `main_test.go`.

Acceptance: startup tests for each failure mode with the teaching message pinned.

### 7d. Gateway-mode conformance
**Spec:** §8.2 (gateway mode), §8.3 items 1–4 · **Dep:** 7b, 1, 8d, 5a

Goal: the full deny sweep and gateway fixtures exist — the safety net for every later auth
slice — running against real enforcement: `auth: on` boots from 8d, so both halves of the
sweep hold from this slice on and no permissive window was ever deployable.

Changes:
- Harness mode `gateway` (env-configured server; identity-injecting helpers from slice 1).
- Deny sweep, complete: every op — no identity → 401 `unauthorized`; untrusted-peer identity →
  401; authenticated-but-ungranted → 403 `forbidden` for grant-protected ops; grant-free ops
  (`describe_server`, `infer_schema`, `list_namespaces`, `whoami`, `capabilities`) succeed
  ungranted; `list_tables` → `not_found` ungranted. Envelope shapes pinned.
- Auth-off invariants: headers ignored even from trusted CIDRs (send them, assert no principal
  anywhere).
- `describe_server`'s `auth: on`-only extension lands here (§2): the response reports the auth
  mode, the enabled identity sources (read-only names — `trusted-proxy` when proxies are
  configured and `admin-key` when `DOLMEN_ADMIN_KEY` is set, initially; `api-keys`
  joins with 10b, `oidc` with 10f; never key material), and the engine capability surface
  inlined verbatim from the `capabilities` op (5a); under `auth: off` the response stays
  byte-identical (§8.1).

Files: `internal/api/ops.go` (describe_server extension), `internal/conformance/` (new
`auth_mode_test.go` + harness extension).

Acceptance: the full sweep runs green in `make test`; op-count table-driven so new ops join
automatically; the auth:on extension pinned (auth-off byte-identical).

### 7e. Namespace-creation gating above the seam
**Spec:** §2 (implicit creation disabled under auth:on), §6.2 global rule · **Dep:** 7b

Goal: the create-on-open era ends **at the seam** — the op layer owns the policy.

Changes:
- `Store.ns()` stops creating: missing namespace = `ErrNotFound` (the §6.2 global rule becomes
  literally true in adapter #1).
- Op-layer `ensureNamespace` helper: under `auth: off` the dispatch path ensures existence before
  engine ops (v0.2.0 semantics preserved through an explicit call — `CreateNamespace` with
  already-exists treated as success); under `auth: on` no ensure — `not_found` **after**
  authorization (order matters, §2).
- The 2b legacy wrappers that tests use collapse into explicit ensures where tests relied on
  implicit creation (contained churn via the helper).

Files: `internal/store/store.go` (ns), `internal/api/` (dispatch helper + op call sites),
store tests (mechanical ensure additions).

Acceptance: auth-off conformance byte-identical (implicit creation still works through the op
layer); gateway-mode tests show `not_found` (post-authz) for absent namespaces (the
gateway-mode pins ride with 7d's fixtures, which run post-8d).

### 8a. Grant registry store
**Spec:** §3 preamble, §3.1–3.2 · **Dep:** 3b

Goal: the server-level grant store exists — durable, above the seam, engine-invisible.

Changes:
- `<data>/_dolmen_registry.db` (leading underscore: cannot match the namespace grammar, excluded
  from listing automatically): `grants(subject_type, subject_id, ns_path, table_name TEXT NOT
  NULL DEFAULT '', drop_gen INTEGER NULL, verbs_json, nsgen BLOB NULL, created_at)` PK
  `(subject_type, subject_id, ns_path, table_name)`. `''` marks a namespace-level grant — the
  table-name grammar forbids the empty string, so the sentinel is unambiguous, and NOT NULL is
  what makes the composite PK enforce uniqueness: SQLite treats NULLs as distinct, so a nullable
  `table_name` would admit duplicate namespace grants and break the idempotent merge. Lifetime
  binding per §3.4: a table grant records `(nsGen, table_name, drop_gen)` — the schema `Version`
  excluded, exactly as everywhere; a namespace grant records the namespace's `nsGen` (`drop_gen`
  NULL); an ancestor grant records the ancestor's `nsGen`; `*` records nothing (both NULL); plus
  a `meta` table reserved for 8e tombstones and 10a keys.
- Store API: put/merge (idempotent, keeps `created_at`, fixed §2 verb serialization order),
  revoke-verbs, subtree/exact queries, subject filters, the §3.2 sort order.
- Open/close beside `Store.Open`; single-writer (WAL + immediate tx, same DSN discipline).

Files: new `internal/store/grants.go`, `internal/store/store.go` (open/close); store tests.

Acceptance: idempotent merge, last-verb-revoke deletes row, subtree semantics, sort order —
pinned by store tests. No ops yet.

### 8b. `grant` / `revoke` / `list_grants` ops
**Spec:** §3.1–3.2, §2 · **Dep:** 8a

Goal: the three grant ops exist and validate per contract (still inert until 8c wires checks).

Changes:
- Request/response shapes per §3.2 (verbs in any order; response serialized in §2 order;
  `revoke` explicit verbs; `list_grants` subtree + exact-subject filters; `{grant: …|null}`).
- Validation: verb enum/dup-free; `*` whole-object only, no segment wildcards; existing-objects
  rule (existence via `NamespaceState`/`TableState`; `*` excepted; table requires concrete
  namespace); subject charset = §1.1.
- `admin`-verb requirement enforced once 8c lands (ops wired behind the check then).

Files: `internal/api/ops.go`, `internal/mcp/server.go`, `internal/api/openapi.go`; conformance.

Acceptance: conformance shape/validation pins; ops absent from dispatch under `auth: off`
(§8.1). (`auth: on` is not bootable until 8d — the pins run at handler level here and
end-to-end from 8d.)

### 8c. Authorization resolution in dispatch
**Spec:** §3.3, §2 (verb table), §6.2 (AuthBinding) · **Dep:** 8b, 7b, 7e, 8e, 6b, 6c, 8f, 9h, 5a, 9g

Goal: deny-by-default becomes real — every op checks its required verb(s) against resolved
grants; the engine guards receive real bindings. Enforcement is complete here; the `auth: on`
boot gate lifts with 8d, once the startup invariants exist. Every dependency is
load-bearing: 7e — with `Store.ns()` still
create-on-open, a covering `schema` grant could materialize a missing namespace through
`create_table`, the §2 bypass the parent-`admin` gate exists to prevent; 8e — grants are
lifetime-bound from the first enforcing revision, else a dropped-and-recreated table's
predecessor grant still authorizes the successor; 8f — without the drop cascade a
predecessor's row survives the drop inert-but-wedged, 409-ing every later re-grant of the
same (subject, object); 9h — authenticated writes must never share a global idempotency
domain, where two principals' same-keyed inserts collide, replay, or suppress each other;
9g — every auth-on filter is row-local from the first enforcing revision, else a data-verb
grant on one table becomes an existence oracle over ungranted tables.

Changes:
- `resolveVerbs(identity, object)` in `internal/api/auth.go`: union over principal subject +
  group subjects × `*` + each ancestor namespace + namespace + table (path walk, depth ≤3 —
  no FGA dependency needed for these semantics; the resolver is the "embedded FGA" the spec
  names, invisible above the ops) — **plus the admin-key identity's implicit `admin` on `*`**
  (§1.3): it is configuration, not data (attached to the credential, never a `list_grants`
  row, gone when the env is removed), surfaced to the engine as a synthetic Root
  `AuthBinding`. Without it the bootstrap kingmaker 403s on `grant` itself and no first real
  administrator can ever be minted.
- Per-op required-verb table (from §2, incl. conjunctive rules: upsert = create AND update;
  drop_table = schema AND admin) enforced in dispatch under `auth: on`; 403 `forbidden`
  envelope; authorization failure fails closed (500, never bypass).
- `AuthBinding` sets computed from matched grant rows and passed to engine calls (the 2a
  zero-values become real here); authz-precedes-existence for `list_tables` (ungranted →
  `not_found`).
- Enforcement is complete here; 7b's fail-closed `-auth on` startup gate stays down one more
  slice — it lifts with 8d, after the source-presence (7c) and usable-root-administrator
  checks exist, so no `auth: on` deployment can boot administratively dead (no usable source,
  or a trusted proxy with no reachable root grant).
- The §2 data-dependent migration list (`set_enum`, `set_row_access` enabling, every
  vectorization change, FTS rebuild/removal paths, `add_field` with backfill or
  required-no-backfill, `drop_field`) additionally requires table-wide `read` under
  `auth: on` — from this slice on: the gate is a verb check and belongs to enforcement (a
  `schema`-only caller must never trigger row-dependent outcomes or embedding-provider
  calls); 9i re-verifies it against the scope machinery and adds §4.3's disclosure rules.
- `describe_table`'s count follows the caller's verbs from here: holders of only
  `schema`/`admin` receive an explicit empty visible set and a count of 0 (§4.3 — every
  table, default tables included); `read`/data-verb holders get the table-wide count until
  9d's scope work refines `row_access` tables.
- The SSE route joins deny-by-default: `/v1/subscribe` checks the §2/§9.3 standing-read target
  (`read` on the selected table(s), or the namespace for unfiltered feeds) at stream open and
  re-evaluates live — an authenticated-but-ungranted caller gets 403 and no frames from the
  first enforcing revision (9d adds per-record owner-label scope filtering when `row_access`
  goes live).

Files: `internal/api/auth.go`, `internal/api/ops.go` (dispatch), `internal/store/engine.go`,
and the concrete operation files that carry the in-transaction checks — `lifecycle.go`,
`store.go`, `insert.go`, `update.go`, `upsert_key.go`, `search.go`, `vector.go`, `query.go`,
`migrate.go`, `changelog.go`, `getrows.go` (2b's ignored guard parameters become enforced
here; the interface file alone activates nothing; 5a is a dependency so `getrows.go` exists
to be guarded). 6c is a dependency because trusted-proxy streams
carry no credential state to revalidate — the age bound is their identity-refresh backstop.

Acceptance: gateway conformance — the 7d sweep's 403s become real; inheritance-down,
union, ancestor-admin delegation cases pinned.

### 8d. Lockout guards + startup root-admin check
**Spec:** §1.2 (usable root administrator), §3.4 (last-admin) · **Dep:** 8c, 7c

Goal: accidental lockout is hard; boot fails without a usable administrator.

Changes:
- Startup: `auth: on` requires a usable root administrator — the admin key **or** a durable
  `admin`-on-`*` grant targeting a principal (group grants never count — with the one decidable
  §1.2 exception: a root group grant counts when an active key's stored groups locally prove
  membership, activating when keys land (10a/10b); `dolmen-admin`-named grants never count;
  API-key reachability joins with 10b; header/OIDC assumed-reachable per §1.2's documented
  exception).
- `revoke` refuses (409, teaching message) when it would remove the last usable root grant —
  including a configured admin key as "another administrator exists".
- 7b's fail-closed `-auth on` startup gate lifts here: enforcement (8c), source-presence
  (7c, transitive), and the usable-root-administrator check (this slice) all exist — no
  `auth: on` revision ever booted permissive or administratively dead (7d's full sweep runs
  from the next slice on).

Files: `main.go`, `internal/store/grants.go` (guard helper), tests.

Acceptance: guard tests for every "last admin" shape incl. the group-grant decoy; startup
failure messages pinned.

### 8e. Grants bind lifetime keys
**Spec:** §3.4 (lifetime identity) · **Dep:** 8a, 4a, 7b

Goal: a grant names the incarnation it was minted against; resurrected names inherit nothing.

Changes:
- Grant rows record the target's lifetime key at grant time (8a's columns activate): table
  grants the full `(nsGen, Table, DropGen)` — `nsGen` alone cannot distinguish a same-named
  successor recreated inside the same namespace (§3.4; `Version` excluded); namespace grants
  record the namespace's nsGen; ancestor grants the ancestor's nsGen; `*` records nothing.
  `grant`/`revoke` verify current-generation currency atomically at mutation (mismatch = 409,
  re-read re-issue).
- The `AuthBinding` verification paths land with 8c — which depends on this slice — so
  grant-based authorization is lifetime-bound from its first enforcing revision: targeted
  grants mismatch successors; inherited grants verify the ancestor's generation while receiving
  the target's current one.

Files: `internal/store/grants.go`, `internal/api/auth.go`; tests.

Acceptance: drop-and-recreate a namespace/table; the old grant neither authorizes nor blocks the
successor — pinned end-to-end in gateway mode.

### 8f. Drop cascade: write-ahead tombstone
**Spec:** §3.4 (drop cascades grant deletion, crash-atomically) · **Dep:** 8b, 8e

Goal: dropping an object deletes its grants (and its subtree's) in the same operation,
coordinated by the §3.4 write-ahead tombstone from the start — a cascade without its tombstone
has a crash window that leaves a live object grant-less or a recreated successor inheriting the
predecessor's grants, both forbidden. The full §3.4 protocol lands here — tombstone, mutation
refusal, evaluation hold, removal, recovery; 8g is the exhaustive crash matrix and window
pinning.

Changes:
- `tombstones` table in the grant registry: recorded FIRST (capturing the exact covered grant
  set and the target's lifetime key — `nsGen` for a namespace, `(NsGen, Table, DropGen)` for a
  table); engine deletion runs; the captured set is physically removed on completion — exactly
  that set and nothing else.
- Grant mutations targeting a subtree with a pending tombstone fail `409` until cleanup
  completes (§3.4): nothing can be minted into the gap between the captured set and the engine
  deletion — a grant minted against the still-live predecessor after capture would otherwise
  survive the drop as a stale row that authorizes the successor (unbound) or blocks re-granting
  (bound).
- Evaluation hold while pending: the captured grants leave evaluation the moment the tombstone
  records, and authorization checks touching the covered subtree are held behind the drop's
  serialization boundary (retryable `409`-family) — never a bypass (after engine deletion
  commits and before removal, a recreated same-named successor must not inherit the
  predecessor's grants) and never an observable provisional denial (a rolled-back drop
  restores evaluation; no request ever sees the in-between).
- Recovery on open: a pending tombstone finalizes when THAT lifetime is gone (even with a
  same-named successor already recreated) and rolls back (restoring evaluation) when the object
  is alive — every crash point converges to no resurrectable grants and no silently-denied
  successors.
- Confirm-flow responses report the grant count dying with the object (additive response
  field).

Files: `internal/api/ops.go` (drop ops), `internal/api/auth.go` (evaluation hold),
`internal/store/grants.go` (tombstones + recovery + mutation refusal); conformance.

Acceptance: gateway conformance — recreation starts with a clean grant slate; confirm count
correct; grant mutations during the pending window 409; evaluation on a pending subtree holds
(409 — not a bypass, not an observable provisional denial); a crash-recovery pin (kill between
tombstone and deletion → recovery converges).

### 8g. Drop cascade: crash matrix + window pins
**Spec:** §3.4 (recovery convergence; never-observable exclusion) · **Dep:** 8f

Goal: the §3.4 protocol shipped by 8f is verified exhaustively — injected crashes at every
window and the pending-window behaviors pinned end-to-end.

Changes:
- Injected-crash matrix: kill points at tombstone capture, between capture and engine
  deletion, after deletion before removal, during removal, and mid-recovery (including a
  concurrent recreate after deletion) — every point converges to no resurrectable grants and
  no silently-denied successors.
- Pending-window conformance pins: evaluation holds (retryable 409 — never a bypass, never an
  observable provisional denial) and mutation refusals (409), end-to-end in gateway mode.

Files: `internal/conformance/`, `internal/store/grants.go` (crash-injection helpers); tests.

Acceptance: the full matrix green; both window behaviors pinned.

### 9a. `row_access` annotation + `owner` column
**Spec:** §4.1 · **Dep:** —

Goal: the schema machinery for `row_access: "own"` — annotation, implicit `owner` column,
reservation — exists. The public surface stays OFF: `create_table` keeps rejecting the key as
an unknown field in both modes until 9j turns scope enforcement on, so no main revision ever
holds a `row_access` table whose paths do not enforce the visible set (stamping is 9c, CRUD and
feed enforcement 9d, searches 9e/9f, filter allowlist 9g, idempotency domains 9h, guards 9i;
the flip that makes the annotation publicly usable is 9j's opening step, with the query gate).

Changes:
- `TableSchema.RowAccess` (omitted = none); DDL adds `"owner" TEXT` when declared; `owner`
  reserved exactly where the column exists (caller fields, add/rename targets);
  `describe_table` omits `owner` from `fields` but reports the annotation.
- Dispatch gate: the `row_access` key on `create_table` remains an unknown-field rejection in
  both modes (under off permanently, byte-identical §8.1; under on until 9j) — the machinery is
  pinned by store/schema-level tests here, not through the public op.

Files: `internal/schema/schema.go`, `internal/store/store.go` (DDL), `internal/api/ops.go`,
`internal/api/server.go` (schema surface); tests.

Acceptance: store/schema-level pins — column present/absent per declaration, reservation
rules; `create_table` still rejects the key in both modes (9j's flip is what makes it accepted
under `auth: on`).

### 9b. `set_row_access` migration
**Spec:** §4.2 · **Dep:** 9a

Goal: the recovery hatch and the enable path, with their gates.

Changes:
- Migration op `set_row_access` (+ explicit `value`); enabling rejected on populated tables
  (row-existence check) and requires table-wide `read` **before** the populated test (§4.2
  ordering — the rejection is only ever seen by authorized-to-know callers); disabling requires
  `admin` + table-wide read (widens every data-verb holder's reach — §4.2 final wording);
  disabling keeps the physical column.
- The migration is implemented here but **absent from dispatch until 9j** — a table can only
  become `row_access` when enforcement exists (the same gate as 9a's create key).

Files: `internal/schema/schema.go` (Change), `internal/store/migrate.go`,
`internal/api/ops.go`; tests.

Acceptance: all §4.2 cases pinned (store-level here; end-to-end through dispatch from 9j on)
incl. the empty-table enable and the gate ordering.

### 9c. Owner stamping + typed reads
**Spec:** §4.2 · **Dep:** 9a, 7b

Goal: every row-insert path stamps the principal; reads surface `owner` like `id`/`created_at`.

Changes:
- `WriteOpts.Owner` (declared in 2a) flows from the op layer under `auth: on` into the insert
  branches only — insert and both upsert insert branches; callers can never supply `owner`
  (not a request field — automatic via DisallowUnknownFields). Delete- and update-event labels
  are different: each record captures the affected row's own owner, read from the materialized
  rows before the write (4c's rule — a table-wide writer touching another user's row must not
  relabel it, or the original owner's scoped feed misses the event; the writer is not the
  row's owner).
- Projection (`typed.go`) surfaces `owner` on row reads/search results for tables with the
  column; NULL-owner rows (written under auth:off) read as NULL.

Files: `internal/store/insert.go`, `update.go`, `upsert_key.go`, `typed.go`,
`internal/api/auth.go` (opts); tests.

Acceptance: stamping on every write path; `owner` visible in reads, never in declared fields.

### 9d. RowScope computation + CRUD and feed enforcement
**Spec:** §4.3, §2 (feed verb rows), §9.3 · **Dep:** 9c, 5a, 5c, 5d, 6b, 8c

Goal: the visible set is computed above the seam and enforced below it — for row CRUD and the
realtime feeds in the same slice, so no revision exists with CRUD enforced but feeds leaking
(§2 grants data-verb holders table-filtered feeds — own rows only). The public flip itself
waits for 9j, when every scoped path enforces: searches 9e/9f, filter allowlist 9g, idempotency
domains 9h, guards 9i.

Changes:
- Scope resolution in the op layer per §4.3 (read → table-wide; data verbs without read on
  `row_access` tables → `{owner = principal}`; schema/admin-only → empty); scope + the
  incarnation it resolved against (`TableState` snapshot) passed to every engine call — nil
  scope ≠ empty scope.
- Engine: `Update`/`Delete` materialize visible ids first (the existing temp-table pattern
  becomes the materialization boundary — §4.3's security-barrier rule); `upsert_by_key` treats
  invisible natural-key matches as no-match; `GetRows` drops invisible ids; `describe_table`
  counts the visible set.
- Realtime feeds enforce the same visible set (5c's deferral ends here): `ChangesSince`/
  `WaitFor` deliver only records whose `owner` label (stamped since 9c) and table lifetime
  (§9.3) pass the caller's scope; the subscribe handler's `Listen` registration passes the §6.2
  `liveAuthz` re-resolver — per-event scope + incarnation, mid-subscription grant revocation
  teaching-closes the stream (§9.3). Per-event **credential** revalidation rides the same
  callback: API-key state is rechecked per event once keys exist (10b), and token-backed
  streams close no later than the token's `exp` — a deadline armed at stream open, firing the
  teaching close even when no event arrives — once tokens exist (10e).
- Public flip (deferred to 9j): `create_table` accepting the `row_access` key and
  `set_row_access` joining dispatch happen only when every scoped path enforces — the
  enforcement series 9d–9i lands as machinery with store-level pins, and 9j turns the surface
  on with the `query` gate.

Files: `internal/store/update.go`, `search.go` (Delete + fetch), `upsert_key.go`,
`getrows.go`, `changelog.go` (feed filtering), `internal/store/store.go` (the concrete
`DescribeTable` count), `internal/api/ops.go` (the scope call sites — 2c's zero-value passes
become real here), `internal/api/sse.go` (liveAuthz wiring), `internal/api/auth.go`; tests.

Acceptance: store-level + gateway-mode pins — update/delete touch own rows only; upsert-by-key
invisible-collision creates a second row without leaking; counts scoped; §8.3 item 7's
scope-filtering cases (a foreign row's commit never delivers an event to a scoped caller on
`changes_since`/`wait_for`/`subscribe`; per-event revocation drops the stream) on internal
`row_access` fixtures.

### 9e. Scope in searches + vector predicate
**Spec:** §4.3 (security barrier), §7 (visible set exact) · **Dep:** 9d

Goal: searches operate over the visible corpus through the §4.3 materialization boundary —
predicate conjunction alone is not a barrier: SQL does not guarantee evaluation order, and an
allowlisted expression (`iif(secret = ?, abs(-9223372036854775808), 1)`) evaluated on a foreign
row leaks its value through the error. 9g's allowlist cannot reject this (`iif`/`abs` are
legitimate), so the engine evaluates every caller filter only over already-materialized visible
ids — the same boundary 9d's CRUD paths use.

Changes:
- `SearchVector`: visible ids materialized first; the brute-force scan (`vector.go:65`) and all
  caller-filter evaluation restricted to them — no caller expression ever evaluates against a
  foreign row; `min_score`, `truncated`, `skipped_vectors` computed over visible hits only.
- `SearchFulltext`: FTS candidate ids intersected with the materialized visible set, and the
  caller filter evaluated only over surviving ids before fetch; `truncated` over visible
  matches (the ranking-isolation refinement is 9f).
- `describe_table`/`read_rows` scope from 9d rides here in conformance.
- §7's auth-on scoring tier lands here: canonical cosine (float32-normalized operands,
  binary64 component-wise accumulation, correctly-rounded sqrt, zero-norm → exactly 0, final
  quotient clamped to `[-1, 1]`); ordering and thresholds via `q(s) = floor(s / fl64(1e-9))`
  with `id` tiebreak; `min_score` constrained to `[-1, 1]` (`invalid_request` outside),
  threshold compares `q(s) ≥ q(min_score)`. The auth-off path stays bit-for-bit raw
  (§7/§8.1); conformance pins the buckets, the clamp, and the range rejection.
- §7's execution metadata rides auth-on `search_vector` responses: the canonical
  response-level `execution` field (`"exact"` on adapter #1's brute-force path; the closed
  enum is ready for a declared-ANN engine), absent under `auth: off` (§8.1); OpenAPI/MCP
  schemas updated; conformance asserts presence-and-value under `auth: on` and absence under
  `auth: off`.

Files: `internal/store/vector.go`, `search.go`, `internal/api/ops.go` (execution field),
`internal/api/openapi.go`, `internal/mcp/server.go`; conformance.

Acceptance: foreign rows never surface or displace; the §4.3 error-oracle expression pinned as
a conformance case (a scoped search filter whose allowed expression would overflow on a
foreign row returns no error and leaks nothing); skipped/truncated semantics over the
visible set pinned.

### 9f. FTS visible-corpus ranking
**Spec:** §7 (relevance statistics must not include foreign rows) · **Dep:** 9e

Goal: adapter #1 isolates ranking statistics per visible corpus.

Changes:
- Filter-then-rescore over the shared FTS index: candidates restricted to visible ids, rank
  computed over that corpus (per-scope index partitioning is the documented alternative —
  engine's choice per §7, conformance-verifiable either way). §7's auth-on comparison tier —
  the emitted ranks compared with the same canonical `q()` quantization and `id` tiebreak —
  rides this slice.

Files: `internal/store/search.go`; conformance.

Acceptance: a foreign document cannot reorder or displace visible results — the §7 scenario
pinned.

### 9g. Row-local filter allowlist
**Spec:** §4.3 (row-local filters) · **Dep:** 7a

Goal: filter fragments are restricted to row-local expressions, validated above the seam —
and under `auth: on` the restriction applies to **every** update/delete/upsert/search filter,
not only scoped ones: a caller holding a data verb on table A can otherwise place a subquery
against ungranted table B inside A's filter and infer B through result counts or errors (§2
existence-hiding), nil `RowScope` included — default tables and table-wide readers alike.
8c depends on this slice, so the validator is in force before authorization activates; the
materialization barrier (9d/9e) remains specific to own-row scopes.

Changes:
- New `internal/store/filterlang.go`: tokenize a WHERE fragment (reusing the `query_tables.go`
  scanner primitives), accept only target-table column refs, literals, `?` params, the
  enumerated operator/function lists (incl. the deterministic date/time rules — `'now'` and
  timezone forms rejected); reject subqueries, table references (incl. `__fts`), aggregates,
  everything else — `invalid_request` before execution. Applied under `auth: on` to every
  update/delete/upsert/search filter; unchanged under off.

Files: new `internal/store/filterlang.go`, call sites in `internal/api/ops.go`; tests.

Acceptance: the §4.3 subquery example rejected by the validator — for scoped AND unscoped
(table-wide/default-table) auth-on callers alike; the allowlisted `iif`/`abs` expression
passes validation here; allowlist bounds pinned by table tests. (The safe-execution
assertion — the expression evaluated over the materialized visible set, no foreign-row error
— lives in 9e, which owns the barrier.)

### 9h. Idempotency owner-namespacing
**Spec:** §4.3 (final idempotency semantics) · **Dep:** 9c

Goal: idempotency keys are per-(table, owner); foreign domains neither conflict nor reveal.
This slice is required before authorization activates (8c depends on it): authenticated
writes must never share a global idempotency domain.

Changes:
- `_dolmen_idempotency` gains `owner TEXT NOT NULL DEFAULT ''` (`''` = the legacy pre-auth
  domain — §1.1's charset makes principal strings non-empty, so the sentinel is unambiguous,
  and NOT NULL keeps the composite PK a true uniqueness constraint: SQLite treats NULLs as
  distinct, so a nullable owner would let two racing `auth: off` inserts both record instead of
  the second replaying the first); PK becomes `(table_name, owner, key)` with a one-time
  registry migration; `lookupIdem` consults the caller's own domain only — own-domain hit =
  verbatim replay (payload comparison only against one's own record); miss = insert recording
  the domain; legacy pre-auth records replay only to table-wide readers; auth:off retries
  consult the legacy domain only.

Files: `internal/store/store.go` (DDL + migration), `insert.go` (lookup/insert); tests incl.
auth-transition cases.

Acceptance: the §4.3 two-case table pinned; no foreign-collision 409 anywhere.

### 9i. Scope/incarnation guards + migration gates
**Spec:** §4.3 (guards), §2/§4.3 (data-dependent migration list) · **Dep:** 9d, 8e

Goal: scope resolution cannot race a migration or a drop; the data-dependent migrations require
table-wide read.

Changes:
- `scopeIncarnation` verified inside every scoped operation's transaction (extend the existing
  consistent-or-stale pattern from version+dropgen to the full `Incarnation`); mismatch → 409
  retry.
- The §2 data-dependent-migration read gate (enforced in dispatch since 8c) is re-verified
  here inside the ordering §4.2/§2 pin — before any data-dependent check or provider call —
  and §4.3's response-disclosure rules (visible-set counts, redacted rejections) join with
  the scope machinery.
- The §6.2 cross-request plan→apply binding: dry-run responses carry the opaque
  `expected_incarnation` token (the plan's `Incarnation`), and under `auth: on` an apply
  carrying any precondition MUST carry it — `expected_version` alone is `invalid_request`
  (version 1 cannot distinguish a same-named predecessor recreated at version 1; a stale
  destructive plan must 409 on the token mismatch). `expected_version` remains the auth-off
  compatibility path.

Files: `internal/store/` (guard in each scoped method), `internal/api/ops.go` (migrate gate);
tests.

Acceptance: race tests (migration concurrent with scoped op → 409-retry converges); each gated
migration 403s without table-wide read.

### 9j. `query` gate + acceptance scenarios
**Spec:** §4.4, §8.3 items 2–6 · **Dep:** 9b, 9d–9i

Goal: raw SQL gated per contract; the epic's acceptance scenarios become conformance; and the
`row_access` surface goes public — the flip 9a deferred lands here, when every scoped path
enforces (CRUD + feeds 9d, searches 9e/9f, filter allowlist 9g, idempotency domains 9h, guards
9i, and this slice's own `query` gate).

Changes:
- Public flip: `create_table` now accepts the `row_access` key under `auth: on` (still an
  unknown field under off) and `set_row_access` joins dispatch — the 9a/9b machinery becomes
  publicly usable exactly when enforcement is complete, and its deferred conformance pins go
  live.
- `query` on `row_access` tables requires table-wide `read` (403 otherwise) — the namespace-level
  gate from 8c already covers the rest.
- Conformance: the umbrella end-to-end scenario (default permissions; write-own-read-own;
  read-only elsewhere; list/create asymmetry; raw SQL denied; upsert non-leak; idempotent
  replay owner-only; `truncated` never leaks) and the append-only telemetry scenario; the
  fail-closed (corrupt grant store → 500, never bypass) and source-blindness subset (runs fully
  in 10g).

Files: `internal/api/ops.go` (gate), `internal/conformance/` (scenarios).

Acceptance: both scenarios green in gateway mode; fail-closed test; the 9a/9b public-surface
pins green from here.

### 10a. API-key registry + ops
**Spec:** §1.5 · **Dep:** 8a, 8d

Goal: source C's storage and ops exist (native-first per the build-order ruling).

Changes:
- `keys` table in the registry db (key_id immutable server-generated, name, principal, groups
  json, hash, state, created_at); CSPRNG mint `dlm_` + 32 bytes base64url (43 chars); shown
  once; identity-shape validation at `create_key` (§1.1 charset, `-max-groups`); `admin`-on-`*`
  required; `dolmen-admin` forbidden; `list_keys` shows state, never credentials;
  `revoke_key` by key ID; hashed storage with constant-time comparison convention.
- The key ops stay **out of dispatch until 10b**: `dlm_` bearer authentication does not exist
  yet, and an intermediate revision must never hand an administrator a credential the server
  itself rejects with 401 — the ops are implemented and tested at handler level here and join
  dispatch with 10b (the same gating the OIDC slices apply to their routes).

Files: `internal/store/grants.go` (registry grows keys), `internal/api/ops.go`; tests.

Acceptance: mint/list/revoke lifecycle at handler level; shape rejections;
never-echo-credentials pinned (dispatch and end-to-end pins from 10b on).

### 10b. Bearer dispatch + self-revocation guard
**Spec:** §1 preamble (shape-selected dispatch), §1.5 guard · **Dep:** 10a, 8d, 9d

Goal: `dlm_` keys authenticate; revocation cannot wedge the deployment.

Changes:
- `resolveIdentity` routes bearer shapes: `dlm_` prefix → key registry (hash lookup;
  unknown/revoked → plain 401, indistinguishable); structural token separators → signed-token
  verification (10e; until then a non-`dlm_` bearer is the admin-key compare only); wrong-shape
  bearer = 401, never reinterpreted.
- `revoke_key` mirrors the last-admin guard at the credential layer under the shared
  registry serialization (the cross-registry lock from 8d); revoking the last active key that
  bears a usable root principal **or locally proves one** — its stored groups satisfy a root
  group grant, §1.2's key-provable exception — is the 409; the same proof updates 8d's startup
  check symmetrically (an active key's stored groups make a root group grant a usable
  administrator from here on).
- The 10a key ops join dispatch — minted keys can authenticate from this revision on.
- Active keys join 7c's source-presence startup check and `describe_server`'s auth-on source
  list (7d's extension).
- SSE streams recheck key state per event through the 9d live callback — a revoked key drops
  the stream at its next event, best-effort immediately (§9.3).

Files: `internal/api/auth.go`, `internal/api/sse.go`, `internal/store/grants.go`; tests.

Acceptance: uniform-401 tests; guard tests incl. the alice/bob cross-check scenario.

### 10c. OIDC provider abstraction + config
**Spec:** §1.4 · **Dep:** 7b

Goal: the PocketBase-shaped provider layer and its config, without the flow yet.

Changes:
- `internal/authn/oidc.go`: provider interface (endpoints + scopes + normalize-to-claims),
  generic OIDC via discovery (`/.well-known/openid-configuration`), optional GitHub preset;
  `DOLMEN_AUTH_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` (secret env-only per convention)
  + optional scopes, validated at startup.

Files: new `internal/authn/oidc.go`, `main.go` (config); tests against a stub issuer.

Acceptance: discovery + preset resolution tests; malformed config startup errors.

### 10d. `/v1/auth/begin` + callback
**Spec:** §1.4 · **Dep:** 10c

Goal: the browser dance — the one deliberate non-JSON surface — implemented but not yet
exposed: the callback cannot hand out a token before 10e's keyring exists, and a temporary
incompatible token is exactly what §1.4's pinned wire format forbids. The routes join the mux
in 10f, when minted principals also carry the pinned issuer-qualified encoding (a token minted
with a bare `sub` could collide across issuers, §1.4); here the handlers are exercised directly
(handler-level tests), never through registered routes.

Changes:
- `GET /v1/auth/begin` → redirect to the IdP authorization endpoint (PKCE + state/CSRF);
  callback exchanges the code and extracts `sub` + groups claims; token minting sits behind the
  10e seam at the point where the page would hand the credential out. Auth:on-only; nonexistent
  under off (absent from dispatch and unauthenticated by construction, §1.2); routes
  unregistered until 10f.

Files: new `internal/api/authflow.go`, `internal/api/server.go` (route registration lands with
10f); tests with the stub.

Acceptance: full dance against the stub at handler level incl. state rejection and PKCE
verification; no route registered yet.

### 10e. Token format + keyring
**Spec:** §1.4 (pinned wire format) · **Dep:** 10d, 8a, 9d

Goal: Ed25519 tokens that verify on every replica and restart, per the pinned format.

Changes:
- `internal/authn/token.go`: keyring persisted beside the registry (minted once); compact JWT
  per §1.4 (`v`, `iss` = deployment's own stable id minted-at-first-startup and persisted,
  `sub`, `grp`, `iat`, `exp`, `kid`); verification (signature under `kid` with rotation
  overlap, unexpired, supported `v`, `iss` equality — cloned-config deployments reject each
  other); `DOLMEN_AUTH_OIDC_TOKEN_TTL` (default `168h`, range `1h`–`720h`) +
`DOLMEN_AUTH_OIDC_DEPLOYMENT_ID` pin; bearer presentation in dispatch.
- Routes stay unregistered: a conforming mint also needs the issuer-qualified principal (10f) —
  a token bearing a bare `sub` could match another issuer's grants (§1.4), so the dance goes
  public with 10f, not here.
- Token-backed SSE streams arm an expiry deadline at open and close no later than the token's
  `exp` with the §9.3 teaching close; reconnect with a fresh token resumes from the durable
  cursor.

Files: new `internal/authn/token.go`, `internal/store/grants.go` (keyring persistence),
`internal/api/auth.go`, `internal/api/sse.go` (expiry deadline), `main.go`; tests.

Acceptance: format/verification matrix incl. cross-deployment rejection and rotation overlap.

### 10f. Issuer-qualified principal encoding
**Spec:** §1.4 (`oidc:v1:…`) · **Dep:** 10e, 8d

Goal: principals and groups from source B are stable, injective, versioned strings.

Changes:
- Encoding `oidc:v1:<D>:<claim>` (D = first 26 lowercase base32 chars of SHA-256 over the
  issuer URL bytes); applied to `sub` and group claims at authentication; validated against
  §3.1's charset/length — un-encodable identity = 401 at the source, never a grant that cannot
  be named; `v1` tag never reinterpreted; issuer change = disjoint principal population with the
  §1.2 startup check naming the stale grant; reachability's issuer-qualification branch wired
  into 8d's check.
- The 10d routes (`/v1/auth/begin` + callback) join the mux — the dance goes public exactly
  when minted principals carry the pinned issuer-qualified encoding (the 8d dependency is
  load-bearing: tokens must not be mintable while grant-protected ops are still permissive;
  the root-administrator invariant exists too).
- The OIDC source joins 7c's source-presence startup check: an OIDC-only deployment (no
  trusted proxies, no admin key) counts its enabled, validated provider config as an identity
  source and boots — the check knows proxies and keys only until here; the `oidc` source also
  joins `describe_server`'s auth-on source list (7d's extension).

Files: `internal/authn/oidc.go`, `internal/api/auth.go`, `internal/api/server.go` (routes on),
`main.go` (source-presence check); tests.

Acceptance: encoding table tests; issuer-change lockout scenario pinned at startup; the 10d
dance end-to-end through the registered routes.

### 10g. Native+keys conformance mode
**Spec:** §8.2 (mode 3), §8.3 items 6–7 · **Dep:** 10b, 10f, 1, 9j, 7d

Goal: the third harness mode — the full native story under CI.

Changes:
- Local issuer stub (httptest OIDC server); fixtures run the real dance; key lifecycle
  (create → use → list → revoke → 401); source-blindness matrix — the grant-matrix subset
  runs identically under header identity, OIDC token, and API key for the same
  `(principal, groups)`; realtime cases in all modes (§8.3 item 7 completes).

Files: `internal/conformance/` (stub + mode fixtures).

Acceptance: the full §8.3 list green in all three modes within `make test`.

---

## Closing

### 11. Docs + skills: final polish
**Spec:** §0.5.2 (two knobs), docs stream · **Dep:** all

Goal: the architect-facing guidance that makes the tenancy model teachable.

Changes: the structural-vs-logical two-knobs guide (sub-namespace vs `row_access: "own"` — the
same grant language drives both); deployment guide (gateway tier, native tier, the §1.2
assumptions, `-max-subscription-age`/`-change-retention` knobs); README/skills final pass for
hierarchy, realtime, and auth surfaces. Each slice's DoD carried its own line-level docs; this
slice is the coherent narrative on top.

Files: `README.md`, `skill/dolmen.md`, `skill/dolmen-admin.md`, `docs/`.

Acceptance: docs review; no behavior change.

---

## Not filed — demander-gated (D20/D25)

The Postgres adapter (engine-2), the lakehouse adapter (engine-3, append-dominated tier),
webhooks (§9 layer 4), user-facing export/import, and job-queue/claim semantics. All are
designed in the spec; none has a demander; none is scheduled. The seam (slices 2a–2c) and the
spec-pinned signatures are what keep them cheap to add when one appears.

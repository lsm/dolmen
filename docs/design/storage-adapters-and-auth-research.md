# Storage adapters (Postgres, lakehouse) and auth support — research note

**Research and planning note, 2026-09-14.** No implementation accompanies this document; it
records the storage-adapter and auth feasibility work requested of the research task so the
plans, retrofit-cost findings, and open decisions survive outside the session that produced
them. Code references are symbol-level and reflect `main` at `ba06ba2`.

The design authority remains `identity-and-engines.md` (the #159 spec) and its
`implementation-plan.md`; where this note endorses that spec it says so rather than
restating it. Terminology (adapter #2 = Postgres, adapter #3 = lakehouse, sources A–D,
Lane A/B) follows the spec's vocabulary.

---

## 1. PostgreSQL storage backend plan (adapter #2)

### 1.1 Where the seam stands

The engine seam is landed and threaded. `store.Engine` (`internal/store/engine.go`) defines
25 methods — 9 namespace/table lifecycle, 3 migrate, 6 row CRUD, 2 change-feed, 1 raw query,
2 search, `Capabilities`, `Close` — plus the auth vocabulary: `AuthBinding`, `RowScope`,
`Incarnation`, `WriteOpts.Owner`, and `Listen`'s `liveAuthz` callback. Consumers are
interface-typed end to end: `api.Server.eng` (`internal/api/server.go`), `internal/ops`
(`ops.go`), and the public facade (`dolmen.Store.eng`, root `store.go`). `*store.Store` is
the only implementation, compile-checked by `internal/store/engine_compat_test.go`.

Two concrete pins remain — the only type-level SQLite coupling above the store:

- `api.New` takes `*store.Store`, not the interface (`internal/api/server.go`, `New`).
- The public facade's `dolmen.Open` calls `store.Open` directly (root `store.go`).

Everything above the seam (envelope, dispatch, validation, OpenAPI, MCP tools, SSE) is
engine-neutral by construction: no SQLite specifics exist outside `internal/store`.

### 1.2 What the seam abstracts cleanly (adapter #2 inherits)

- **The typed-value model lives above the driver.** `coerceValue`
  (`internal/store/insert.go`) reduces every write to int64-or-finite-float64 (NaN/Inf
  rejected pre-storage, `finiteNumber`), bool → 0/1, JSON → marshaled string, timestamp →
  canonical string, vector → little-endian float32 blob (`internal/schema/schema.go`,
  `EncodeVector`). An engine that reuses this layer inherits the entire coercion contract.
- **Vector search is exact Go arithmetic.** `SearchVector` (`internal/store/vector.go`)
  fetches `id, blob` and computes cosine in-process. Bit-identical parity across engines
  comes from sharing the `cosine` function, not from the database; pgvector/HNSW is a later
  declared-ANN accelerator (decision D26 already authorizes it).
- **Change log and cursors are engine-owned tables.** `_dolmen_changes` is minted inside
  the write transaction (`internal/store/changelog.go`, `mintChanges`); cursor tokens are
  opaque random hex resolved through a mapping table (`mintCursorToken`/`resolveCursorToken`)
  — the coordinated-storage token form §9.3 permits, portable as plain SQL. Retention and
  chain-cap pruning (`pruneChanges`) are dialect-neutral.
- **Incarnation guards, idempotency (payload-hash compare), drop generations, pagination
  (`LIMIT ? OFFSET ?`), `ON CONFLICT DO NOTHING/UPDATE`** — portable SQL patterns.

### 1.3 What is SQLite-shaped (adapter #2 must reimplement)

Ranked by lift:

1. **FTS5 — the single biggest item.** Today a virtual table
   (`fts5(..., tokenize='porter unicode61')`, `internal/store/store.go` `createFTS`) with
   synchronous shadow-row writes on every insert/update/delete, and `MATCH ? ORDER BY
   rank, rowid` execution (`internal/store/search.go`). §7 pins the FTS5 match grammar
   (core subset, precedence ladder), porter+unicode61 tokenization, and BM25
   (k1=1.2, b=0.75, weights 1.0) — and, under `auth: on`, adapter #1's rank values
   bit-for-bit. The conformance suite pins it concretely (`internal/conformance/search_test.go`,
   `errors_test.go`: syntax accept/reject including `field:term`, `NEAR`, prefix; BM25
   ordering; the bare-`-` teaching message; `fts5: syntax error` verbatim). Viable
   strategy: extract tokenizer+BM25 into shared Go code, store token streams/postings in
   ordinary Postgres tables, and evaluate the core-subset match and rank in Go — exact by
   construction because the same code computes it. tsvector can pre-filter candidates;
   ranking stays Go-side. This is a slice cluster, not a slice.
2. **The raw-`query` dialect and error taxonomy.** Caller SQL executes as SQLite
   (`internal/store/query.go`, `Query`); errors are normalized from modernc message text
   via regexes (`internal/store/sqlerr.go`) into pinned teaching strings (`table %q not
   found; use list_tables…`, `unknown SQL function "no_such_fn"` — pinned in
   `internal/conformance/errors_test.go`). Adapter #2 needs a SQLSTATE-driven translator
   emitting the same user-facing strings — more stable than text sniffing, behind the same
   messages. §7 explicitly leaves the dialect stance to engine-2; see the open questions.
3. **Number storage fidelity.** Pinned hard by `TestNumericFidelityMatrix`
   (`internal/conformance/embedded_parity_test.go`): NUMERIC affinity semantics verbatim —
   fractionless REALs rewritten to INTEGER at storage (so `-0.0` reads back as int64 `0`),
   int64 max/min exact, 17-digit shortest-round-trip doubles, `1e400` rejected,
   exponent-format reconciliation bands, and 2^53+1 exact on wire and facade. Implication:
   Postgres `DOUBLE PRECISION` alone is non-conforming (loses integers beyond 2^53);
   Postgres `NUMERIC` plus a normalize layer (integral numerics → int64, else float64)
   reproduces the affinity contract exactly and keeps NaN/Inf rejection (enforced above the
   driver, plus the SQL-level guard `checkRowValue` in `query.go`).
4. **JSON handling.** Stored as TEXT today, byte-preserving (`coerceValue`'s JSON arm),
   read back with `UseNumber` (`internal/store/typed.go`, `decodeValue`); fixtures pin
   verbatim blob numbers (30-digit pi, `1e-400`) as `json.Number` tokens. Postgres `JSONB`
   normalizes (key order, number forms) and would break golden bodies — adapter #2 should
   keep JSON in `TEXT` for v1; JSONB only as an invisible index accelerator.
5. **Namespace = file.** Listing is a filesystem walk, creation is `O_EXCL` plus the
   `<data>/a/b/c.db` layout, drop is `os.Remove` with a leaf-only check
   (`internal/store/lifecycle.go`). Adapter #2's mapping is pinned by §0.5.1:
   schema-per-namespace, catalog-driven listing, `CREATE SCHEMA`/`DROP SCHEMA` plus a
   descendant query. Medium, mechanical.
6. **DDL details.** `strftime('%Y-%m-%dT%H:%M:%fZ','now')` defaults (the millisecond shape
   is pinned by the harness's `createdAtRe`), `INTEGER PRIMARY KEY AUTOINCREMENT` →
   `GENERATED ALWAYS AS IDENTITY`, WAL/busy-timeout/`_txlock=immediate`/rw-pool-of-1
   (`internal/store/store.go`, `dsn`) → pool and isolation decisions,
   `temp._dolmen_delete_ids`/`_dolmen_update_ids` temp tables → `pg_temp` + `ON COMMIT
   DROP` or CTEs, `LastInsertId` → `RETURNING`, UNIQUE-violation sniffed from message text
   → SQLSTATE 23505, the "duplicate column" sniff in `Migrate` → 42701.
7. **Notifications.** In-process commit listeners plus a 250 ms poll fallback
   (`internal/store/listen_live.go`, `notify.go`). The durable table is the source of
   truth, so the polling design is multi-process-correct on day one; Postgres
   `LISTEN/NOTIFY` is a pure latency optimization later. `Capabilities` stays truthful.
8. **Topology, ordering, and confinement obligations** (spec §0.5.3/§9.3, not SQLite
   code): role-per-schema privileges plus catalog rejection
   (`pg_catalog`/`information_schema` and function-equivalent probes refused on the
   query path). And — subtler — §0.6/§9.3's ordering guarantee does **not** come free
   under MVCC: Postgres sequences and IDENTITY columns allocate at reserve time, so two
   concurrent writers can reserve change-log positions 1 and 2 and commit in the
   opposite order; a client that observes and persists cursor 2 before position 1
   commits has permanently skipped position 1 (`seq > ?` resume never returns it).
   §9.3 requires "the same serialization point that orders commits assigns the
   sequence" — adapter #1 gets that from its single rw connection; adapter #2 needs an
   explicit per-namespace serialization point: a `pg_advisory_xact_lock` keyed on the
   namespace (or an in-transaction counter row it updates), which serializes
   per-namespace writes exactly as SQLite already does while leaving cross-namespace
   concurrency — which adapter #1 never had — intact. Concurrent intra-namespace
   writers with commit-watermark reads (serve only the contiguous committed prefix) are
   a later, conformance-proven option, not the v1.

### 1.4 Can the conformance suite run against multiple engines?

Yes, with harness work. The suite (105 top-level tests, ~7.3k lines, run under
`go test -race ./...` in CI) is already transport-parameterized (HTTP `/v1`, HTTP MCP,
stdio subprocess, embedded facade — all parity-diffed in `parity_test.go`) and mode-shaped
for auth (`harnessMode{name:"off"}`, identity scaffolding, `assertIdentityIgnored` —
`harness_test.go`). But it is engine-hardcoded: `harness.start()` calls `store.Open` at one
site, and at least five files assume SQLite outright — out-of-band FTS5 surgery through the
modernc driver (`outofband_test.go`, `search_test.go`'s stemming test), verbatim SQLite
error strings (`errors_test.go`), NUMERIC-affinity pins (`embedded_parity_test.go`),
SQLite `CAST(... AS BLOB)` alias semantics (`coercion_test.go`), and the 2000-column
rationale in `CreateTable`'s limit error. Harness changes needed:

1. One injection point: parameterize the harness engine constructor (extend the existing
   `storeOpts` mechanism; env-driven, e.g. `DOLMEN_PG_DSN` → run-or-skip).
2. Per-engine fixture policy: tag SQLite-specific tests to run on adapter #1 only; the
   engine-neutral corpus (the large majority — envelope, coercion, limits, realtime,
   parity) runs on both.
3. CI: add a Postgres job with a service container (workflow YAML is test infrastructure,
   outside the prod-line budget). The blackbox suite (`internal/blackbox`, eight staged
   HTTP scenarios) is engine-blind at the protocol level but **not** a free second gate
   as wired: its `hermeticEnv` strips every `DOLMEN_*` variable and boots the subprocess
   with SQLite-style `-data` arguments, so a job-level engine/DSN never reaches it —
   engine/DSN plumbing through the blackbox boot path and its restart helper is part of
   the matrix slice, not a given.

One sequencing note: §7's `q(s)=floor(s/1e-9)` canonical quantization is designed but built
nowhere (the current suite pins raw, unclamped cosine — the auth-off tier). Cross-engine
*score*-parity claims formally rest on the auth-on tier; a Postgres engine landing before
Lane B should gate on the auth-off corpus (byte-parity via shared arithmetic) and treat the
quantization slice as a shared dependency of Lane B and adapter #2.

### 1.5 Phased slice plan (~100 prod lines per PR)

The SQLite store is ~5.5k prod lines — the size anchor for what follows:

- **Phase 0 — seam and harness prep** (2-3 slices): engine injection at constructors plus
  an `DOLMEN_ENGINE` knob; harness engine parameterization and fixture tags; extraction of
  the shared value layer (coerce/decode/cosine/projection sizing/limits) from
  `internal/store` into an importable subpackage — behavior-invariant moves, proven by the
  unchanged suite.
- **Phase 1 — core engine** (7-8 slices): skeleton (open/pool/schema-per-namespace registry
  DDL + nsgen) → namespace lifecycle → table DDL → insert + idempotency + change minting →
  typed reads and number normalization → update/delete/upsert paths → `query` plus the
  Postgres error translator → migrate port.
- **Phase 2 — search** (3 slices): FTS tokenizer/BM25 extraction and postings storage;
  `SearchFulltext` wiring; `SearchVector` exact (near-free reuse).
- **Phase 3 — realtime** (2-3 slices): `ChangesSince`/cursors; `Listen` polling and
  sessions; retention pruning.
- **Phase 4 — confinement and hardening** (2-3 slices): roles and catalog rejection; the
  CI Postgres matrix; topology declaration.

Total ≈ 17-20 slices. The first three, concrete:

1. **Harness engine parameterization** (enabler): widen `api.New` to `store.Engine`, add
   the engine selector to `harness.start()`, tag the five SQLite-hardcoded files. Prod
   delta ~15 lines; test infrastructure otherwise.
2. **Shared value-layer extraction**: move `coerceValue`/`finiteNumber`/`decodeValue`/
   projection sizing/`cosine`/limit constants into a subpackage both engines import; the
   suite is unchanged and green.
3. **Postgres skeleton**: open/close, DSN config, schema-per-namespace registry DDL port
   (strftime → now()-based millisecond defaults, IDENTITY), nsgen, `NamespaceState`/
   `TableState`; the conformance namespace/lifecycle subset green under the Postgres
   engine (locally via DSN env; the CI job behind the same knob).

**What I'd slice first:** the three above, in that order — low-risk, behavior-invariant,
and every later slice depends on them. Nothing in Phase 1+ should start before the harness
can express "run the corpus on engine X", or adapter #2 drifts unpinned.

**Open questions for the principal:**

1. Dialect stance for `query` — accept Postgres SQL on adapter #2 (fork the dialect
   fixtures; `query` becomes engine-documented) vs. translate a SQLite-compatible subset
   (a shared evaluator à la §4.3's filter allowlist). This is the one real contract
   decision; §7 defers it to engine-2. Recommendation: accept Postgres SQL and document
   per-engine dialect — translating arbitrary caller SQL is a money pit, and the allowlist
   already gives scoped filters a portable lane.
2. FTS strategy — shared-Go BM25 (recommended; honors the bit-for-bit pin) vs. relaxing
   the auth-on rank pin for adapter #2 (a spec change). Affects ~3 slices.
3. Demand — D25 gates adapter #2 behind a demander, yet the spec calls Postgres "the
   reference shared engine." Does this research precede a build decision (Phase 0 becomes
   real tasks), or does the demander rule stand?
4. Should the §7 quantization slice (`q(s)`) be pulled forward as a shared dependency of
   Lane B and adapter #2, so cross-engine parity is enforceable before either lands?

---

## 2. Lakehouse adapter feasibility (Iceberg/Delta — adapter #3)

### 2.1 What the spec already settles

D25/§0.5.1 position adapter #3 precisely: a first-class engine for the append-dominated
tier (events, telemetry, memory — the bulk of agent writes), not the operational store; the
small-write problem is solved below the seam via deletion vectors (Iceberg V3/Delta),
merge-on-read (Hudi), LSM (Paimon), or a WAL in front of Parquet; reads go through a
serving/cache tier; high-frequency OLTP is out of scope; full-text is the one genuine gap
(a sidecar implementing the FTS core subset); demand-gated like webhooks (D20). The
assessment below confirms the seam accommodates this, with one open contract decision
(`query`, §2.3).

### 2.2 Op-by-op mapping

- **Natural fits:** `insert` (append + snapshot commit — the format's native op); DDL
  migrations add/drop/rename field (Iceberg schema evolution is a strength);
  `search_vector` exact (Parquet column scan; brute-force cosine in Go scales to moderate
  corpora before an ANN index is needed); describe/list/capabilities (catalog reads).
- **Workable via below-seam machinery:** `update`/`delete`/`upsert`/`upsert_by_key`
  (deletion vectors / merge-on-read — "rare updates are not a blocker" per D25);
  `read_rows` (needs row-group stats pruning or the serving tier); idempotency (a metadata
  table); `set_enum`/backfills (full scans/rewrites — heavy but rare, consistent with
  rare-DDL semantics).
- **Genuine gaps, stated plainly:**
  - `query` (raw SQL) — no SQL engine lives in a table format. §9.3 permits only
    `subscribe` to be declared unavailable, and §7 makes search semantics contract on
    every engine, so an engine refusing `query` is a contract revision, not an additive
    capability field. The conforming paths: implement it below the seam (embed DuckDB, or
    front an external Trino/Spark — see the effort classes below), or amend the spec
    explicitly — §0.5.3, §2's op table, and the shared conformance corpus, which replays
    every op (`parity_test.go`) — to permit raw-SQL-less engines. That amendment is an
    open decision (below), not something this note assumes.
  - `search_fulltext` — sidecar per the spec. The Postgres engine's shared tokenizer/BM25
    extraction (Phase 2 above) is exactly the component the sidecar reuses — the two
    adapters share this cost.
  - `changes_since`/`wait_for`/`subscribe` — snapshot-diff can serve as the change log,
    but the contract's per-namespace gap-free monotonic cursor and owner labels (§9.3,
    `ChangeRecord.Owner`) are not native concepts; they need engine-internal metadata
    (owner stamped into the write path; cursor = an opaque (snapshot, position) token).
    Capability-degraded `wait_for` stays mandatory (never unavailable; degrades to
    scanning, §9.3).
- **Read vs write path split:** writes use the format's optimistic-concurrency commit
  protocol — conflicts surface as `409`, already the contract's collision rule (§0.6);
  reads use the serving tier or direct Parquet scan. Both below the seam, dolmen-blind.

### 2.3 Contract impact and effort classes

**Contract: engine-neutral only if both gaps are implemented below the seam.**
Full-text is contract on every engine (§7's core subset) — D25's own answer is the
sidecar, so `SearchFulltext` stays available and needs no unavailability declaration.
`query` has the same shape: either it is implemented below the seam (DuckDB embed or an
external-engine front), or the spec is amended to permit raw-SQL-less engines — and that
amendment is a **breaking contract revision** (§0.5.3, §2's op table, and the shared
conformance corpus, which replays every op), not an additive capability field: §9.3
allows only `subscribe` to be declared unavailable, and an engine that errors `query`
changes the behavior of requests the current contract accepts. Everything else (RowScope
predicate conjoinment, incarnations, envelope) is already engine-mechanism-agnostic.

**Rough effort classes:** design + gap analysis S; append write path M; update/delete via
deletion vectors M; change-feed mapping M; FTS sidecar M (shared with adapter #2);
read/serving tier L; the `query` story M–L (DuckDB embed). Aggregate: a lane comparable to
or larger than the Postgres engine — quarters, not weeks. The top feasibility risk is the
Go ecosystem, not the contract: `apache/iceberg-go` is early; parquet-go is solid for
reads, but a production Iceberg *writer* in pure Go likely means a sidecar process
(Java/Rust) or a WAL-fronted minimal writer of our own — which is one of D25's sanctioned
below-seam strategies.

**What I'd slice first:** nothing in-repo (the D25 demand-gate stands). Concretely, when a
demander appears: (1) a one-page decision note on the `query` contract question —
implement below the seam vs. an explicit spec revision (the only contract touch, §2.3);
(2) a throwaway spike evaluating iceberg-go/parquet-go write+read against one
conformance-shaped table, to retire the ecosystem risk before any lane is planned.

**Open questions for the principal:**

1. Is the intended consumer the *analytics* property (Spark/Trino/DuckDB reading the same
   Parquet) or dolmen-side durability/scale? It changes whether the serving tier is a v1
   or a v3 concern.
2. `query` on this tier: implement below the seam (embedded DuckDB — the additive path),
   or amend the spec to permit raw-SQL-less engines (an explicit breaking revision to
   §0.5.3/§2 plus the conformance corpus, landed spec-first per its own deviation rule)?
3. Is a non-Go sidecar process (Iceberg writer) acceptable operationally, given the
   local-first single-binary ethos?

---

## 3. Auth support — research and plan (no implementation)

### 3.1 Current state: designed in full, built not at all — and the expensive parts are pre-paid

The design authority is complete (`identity-and-engines.md` §1–§9; decision index D1–D26)
and pre-sliced (`implementation-plan.md`, Lane B = slices 7a–10g, ~24 slices). Production
code: zero — no `-auth`/`DOLMEN_AUTH`/`-trusted-proxies` anywhere (the flag list in
`cmd/dolmen/main.go`), no grant ops among the 23 ops (`internal/api/ops.go`), no
`Authorization` handling (the only hits are the CORS allow-header in `server.go` and the
"no authentication" warning in `main.go`).

What *has* landed is Lane A's enabling surface — precisely the retrofit-expensive part,
already paid:

- The full auth vocabulary sits in the seam (`engine.go`): `RowScope`/`Incarnation`
  parameters on every data method.
- The `owner` column and its scan plumbing exist in `_dolmen_changes` (`changelog.go`);
  `WriteOpts.Owner` is plumbed but read nowhere yet — by design until slice 9c.
- `Listen`'s `liveAuthz` per-event authorization is implemented and consumed
  (`listen_live.go`).
- The subscription age bound (`-max-subscription-age`, the source-A identity-refresh
  backstop) shipped (`main.go`, `internal/api/sse.go`).
- The conformance harness is auth-mode-shaped with off-mode identity scaffolding and
  `assertIdentityIgnored` already enforced (`harness_test.go`) — slice 1's off half exists.

### 3.2 Where the seams sit

- **Single choke point covering both surfaces:** `api.Server.Dispatch` — MCP `tools/call`
  funnels through it too (`internal/mcp/server.go`). Auth placed there gates `/v1/{op}`
  and MCP tools in one move; the principal rides `context.Context` (the `requestIDKey`
  precedent in `internal/api/envelope.go`).
- **HTTP whole-server wrap:** alongside `OriginGuard` in `cmd/dolmen/main.go` — covers
  `/v1`, `/v1/subscribe`, and `/mcp` in one place; `/healthz`, `/version`, `/skills`,
  `/v1/openapi.json` exempted per §1.2 (unauthenticated by recorded decision), plus —
  when source B is enabled — `/v1/auth/begin` and its callback, which §1.2 exempts by
  construction ("they *are* the authentication"): the browser starting the flow holds
  no dolmen credential yet, and a wrapper that 401s them deadlocks the flow before it
  can mint one.
- **MCP specifics:** streamable-HTTP is a stateless JSON-RPC POST (no sessions despite
  `MCP-Session-Id` in the CORS allow-headers); `initialize` carries no identity today; a
  bearer rejected at the HTTP edge returns a plain `http.Error`, not a JSON-RPC error — an
  error-shape decision to make deliberately (below). stdio bypasses HTTP entirely
  (`internal/mcp/stdio.go`) — inherently local trust.
- **Config conventions established:** env-twin flags (`envOr`), env-only for secrets
  (`DOLMEN_EMBED_API_KEY`), functional options for server attachments
  (`WithMaxSubscriptionAge`-style) — a `WithAuthn`-style option slots straight in.

### 3.3 The recommended design (endorse the spec as written)

Four additive identity sources behind one seam, downstream source-blind (§1): A —
trusted-proxy headers (`X-Dolmen-Principal`/`X-Dolmen-Groups`, CIDR-gated on the TCP peer;
v1), B — native OIDC (Ed25519 signed tokens, PKCE, principal = `sub` never email,
`/v1/auth/begin`; built on demand), C — API keys (`dlm_…`, hashed, per-key IDs, machine
groups), D — the `DOLMEN_ADMIN_KEY` bootstrap kingmaker. Six CRUD verbs; embedded-FGA
grants above the seam (engines stay grant-blind); `row_access` tables with a
server-stamped `owner`; visible-set → `RowScope` predicate with incarnation guards;
`auth: off` byte-identical to v0.2.0 permanently. This is minimal-but-elegant in the
spec's own sense: nothing user-shaped to leak, the IdP owns verification, and every
failure teaches. No seam in the codebase contradicts it.

**MCP-spec alignment** (spec 2025-06-18: an HTTP MCP server is an OAuth 2.1 *resource
server* — it advertises `/.well-known/oauth-protected-resource` pointing at an
authorization server; clients use PKCE and metadata discovery, with dynamic client
registration recommended). Mapping: gateway deployments (source A) advertise the external
AS the gateway fronts — the conforming answer for MCP-native clients today, because
metadata only helps if the advertised AS will actually complete an authorization-code +
PKCE exchange against the client's redirect URI. Source B as specced (§1.4) is a **human
browser flow** (`/v1/auth/begin` → IdP → callback → a page that hands the token out); it
mints an OAuth-shaped bearer JWT but is not an authorization server MCP clients can run
the code flow against — serving the two well-known documents plus DCR on top of §1.4 as
specced would advertise an AS that discovery-based clients still cannot obtain a token
from. Making dolmen the advertised AS therefore means extending source B into a full
token-broker AS (authorization endpoint, token endpoint, client registration, redirect
URI handling) — a scope increase over §1.4, not a metadata slice. API keys are non-OAuth
bearers — legitimate machine-tier credentials, invisible to spec-driven discovery,
documented as such. The `/mcp` and envelope gating under `auth: on` (§1.2) already
matches.

### 3.4 What today's deferral bakes in — retrofit-cost audit

**Cheap / already paid:** everything in §3.1. The `auth: off` default is itself the
design — no contract debt accrues while it stands.

**Costs that grow the longer auth waits:**

1. **Op-surface growth.** Every new op is another 9d-threading site. 23 ops today, all
   passing nil scopes. The discipline that keeps slices 8c/9d at one slice each: land new
   ops with their scope parameters already plumbed (they exist in every signature) — zero
   extra lines now, no hunt later.
2. **SQLite-dialect fixture accretion** — the same force as §1: every golden fixture
   exercising SQLite-only SQL in filters/`query` hardens the future shared-evaluator
   requirement (9g) and the Postgres engine's translator. §4.3's filter allowlist already
   pins SQLite evaluation semantics ("one shared evaluator, not just a shared validator")
   — each new pinned SQLite-ism is future cross-engine tax.
3. **MCP error-shape drift.** The HTTP-edge-vs-JSON-RPC auth-error decision gets more
   expensive the more MCP clients accrete on today's permissive behavior. Decide the
   shape in the design note before the identity middleware lands (reading of the spec:
   `401` with `WWW-Authenticate` at the HTTP edge for non-JSON-RPC-aware callers, a
   JSON-RPC error for in-band rejections — pin it in slice 7b).
4. **stdio policy.** No identity source exists on stdio. The spec is silent; options are
   forbid-under-`auth: on` (startup error), a `-principal` flag for local-trust testing,
   or exemption with documentation. Needs a one-line spec amendment before Lane B lands,
   or implementers will diverge.
5. **Multi-replica coupling with §1.** The grant registry's placement is topology-bound
   (§3 preamble): server-level SQLite is correct only for the single-process topology. If
   adapter #2 lands while auth is off, the coordinated-registry requirement arrives with
   the first multi-replica deployment, not before — sequence the Postgres engine's
   confinement slices with that in mind (nothing to pre-build now).
6. **Idempotency owner-namespacing (9h):** `_dolmen_idempotency` has no owner column
   today — a cheap registry migration whenever it lands; no action now.

**Verdict:** the deferral is nearly cost-free in contract terms — the spec pre-paid it.
The real curve is items 1–2: op-threading discipline and dialect-fixture discipline, both
shared with the engine work.

**What I'd slice first (when the posture lifts — not before):** the Lane B opening pair as
already planned — 7a (auth config plumbing: `-auth`/`DOLMEN_AUTH`, `-trusted-proxies`,
`-max-groups`, startup validation) then 7b (identity middleware: header source + admin
key, the 401 `unauthorized` envelope, principal-on-the-log-line); 7c and 7e follow, both
depending only on 7b. Gateway-mode conformance (7d) is **not** an early slice: the
implementation plan pins its dependencies as 7b + 8d — `auth: on` boots from 8d, so the
grant-backed 403/allow matrix cannot run until the grant chain (8a registry → 8b ops →
8c dispatch enforcement → 8d guards/activation) has landed. Handler-level identity
behavior stays testable from 7b; the full 7d suite lands after 8d. API keys (10a/10b)
can ride earlier than OIDC if machine identity is the near-term need (§8.2 notes no
dependency).

**Open questions for the principal:**

1. When the lift happens, confirm gateway-first order (7-series before 8-series) — or is
   API-keys-first (10a/b) wanted for agent fleets before the gateway tier?
2. stdio under `auth: on`: forbid, `-principal` flag, or exempt-and-document? (Needs a
   spec line before Lane B.)
3. MCP-native clients under source B: extend §1.4 into a full token-broker authorization
   server (authorization + token endpoints, client registration — a scope increase), or
   leave MCP discovery pointing at the gateway tier's external AS and keep source B the
   human browser flow?
4. Does the Postgres-engine recommendation in §1 change auth sequencing — engine work and
   Lane B interleaved (they share the quantization and shared-evaluator slices), or
   strictly serialized?

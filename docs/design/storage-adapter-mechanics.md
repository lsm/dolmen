# Storage adapter and MCP-discovery mechanics — implementation spec

**Extracted 2026-09-15 from `storage-adapters-and-auth-research.md`** (PR #312) after ten
review rounds converged on these two clusters: the mechanics below are implementation
specifications, enforced by the implementing PRs' tests — not by further prose review
rounds. The research note carries the design-level conclusions and marks these
"detailed in implementation"; this file is the detail. Terminology follows
`identity-and-engines.md`.

---

## 1. Lakehouse adapter (#3) — per-op mechanics

**Row-id allocation.** The implicit `id` needs a server-assigned, monotonic,
never-reused allocator; Iceberg/Delta have no native equivalent. Ids are assigned inside
the namespace-level serialization point (below), never derived as `max(id)+1` from a
snapshot — that collides under concurrent writers and corrupts `read_rows`, change
records, and idempotent replay.

**Idempotency atomicity.** The idempotency record — key, payload hash, assigned ids —
commits inside the same atomic serialization point as its rows, never a
separately-committed metadata table: a crash between two independent commits duplicates
rows or replays phantom ids (§0.6 violation).

**Namespace commit log — atomicity mechanism.** Iceberg/Delta snapshots order commits
within one table; a namespace-wide feed spans tables, so a `(snapshot, position)` cursor
cannot provide the per-namespace gap-free order (the same reserve-vs-commit skip race as
a Postgres sequence). The log must be committed atomically with every table mutation,
via one of: a transactional catalog/coordinator that commits log and data snapshot
together (JDBC/Postgres-backed catalog); or a dolmen-owned namespace WAL as the
authoritative commit record — WAL append is the commit point, snapshots are idempotent
materializations, recovery replays. Absent one of these, the feed contract is a
revision, not an implementation choice.

**Namespace WAL — read visibility.** The WAL is the read-authoritative tail, not merely
a recovery log: reads (`read_rows`, `query`, the searches) overlay
committed-but-unmaterialized WAL entries as a union view, or the serving view advances
synchronously before the write acks — otherwise an acknowledged write exists only in the
WAL and §0.6 read-your-writes is violated until materialization.

**Owner labels.** `ChangeRecord.Owner` metadata is stamped in the write path
(engine-internal); not a native format concept.

**Optimistic-concurrency handling.** A lost metadata race is refreshed/rebased and
retried internally within bounds; only exhausted or genuinely non-retryable conflicts
surface as `409` (§0.6 transparent-serialization-first — surfacing every routine race
would fail serializable concurrent appends).

**Fulltext.** Sidecar implementing the FTS core expression grammar (D25), with engine-native
text analysis and ranking documented per §7/D27. The 2026-09-19 decision removes
the previously planned dependency on a shared PostgreSQL tokenizer/BM25 extraction.

---

## 2. MCP authorization discovery — mechanics (auth: on)

Model: dolmen is an OAuth 2.1 resource server (MCP spec 2025-06-18). Discovery is
served under exactly one of two ownerships:

**Source A (gateway).** The gateway owns both the protected-resource metadata and the
challenge: dolmen emits a bare HTTP `401`. The gateway also consumes and **strips the
verified bearer before forwarding** — a forwarded external access token hits §1's
fail-closed bearer precedence and is 401'd rather than accepted as the proxy assertion.
Validating or exchanging external tokens inside dolmen would be a new identity source —
out of scope.

**Dolmen-served discovery.** When dolmen knows the AS — via the explicit validated
external-AS URL setting, or source B **after its extension into the token-broker
authorization server** (unextended source B accepts no upstream IdP tokens and issues
none of its own; no usable `authorization_servers` value exists) — dolmen serves:

- `/.well-known/oauth-protected-resource` at the **path-derived** URI of the public
  resource: RFC 9728 derivation from the *public* URL — `/.well-known/oauth-protected-resource/mcp`
  for `/mcp`, and `/.well-known/oauth-protected-resource/dolmen/mcp` when `-prefix=/dolmen`
  makes the public resource `/dolmen/mcp`. Hard-coded paths are wrong under a prefix, and
  host-root metadata routes must be registered outside the `withPrefix` wrapper (which
  rejects host-root paths when a prefix is configured).
- The `401` challenge carries the `WWW-Authenticate` `resource_metadata` link to that
  URI — a bare 401 leaves a standards-based client unable to learn the AS.
- The metadata route itself is unauthenticated (it *is* discovery).
- The metadata response's **REQUIRED `resource` member** carries the same canonical
  public resource URL that derives the metadata path (e.g.
  `https://host/dolmen/mcp` under `-prefix=/dolmen`) — the URL the client actually
  requests; covered by the implementing tests.

**Slice gap.** No Lane B slice covers the endpoint, the challenge linkage, or the
external-AS URL setting — flagged for the implementing epic.

---

## 3. Postgres adapter (#2) — SQL surface mechanics

**Placeholder rebinding.** Dolmen's public query/filter contract pins SQLite-style `?`
parameters; Postgres grammar and drivers use numbered `$N`. Adapter #2 rewrites
placeholders through a lexer-aware rebinding layer — literal strings, quoted
identifiers, and comments are skipped; each `?` becomes `$k` in positional order; and
server-appended pagination parameters (`LIMIT ? OFFSET ?`) are appended to the same
renumbered sequence. Implementing tests pin: mixed user + appended parameters, `?`
inside string literals and comments (never rewritten), and error positions.

**Physical-name mapping (amended 2026-09-19).** Dolmen names may reach 64
characters; PostgreSQL identifiers are limited to 63 bytes. Namespace paths map to
registered generation-specific physical schemas. Table/field names within 63 bytes
normally pass through; longer names first try `<prefix>_<h>`, where `<h>` is 16 hex
characters of SHA-256 and `<prefix>` keeps the whole identifier within 63 bytes.
This truncated hash is not injective. Detect collisions with other mapped columns and
existing PostgreSQL relations, including generated indexes and sequences; allocate a
distinct salted candidate and persist the resulting mapping. Allocation fails rather
than silently aliasing when its bounded retry budget is exhausted.

Generated SQL uses the persisted mapping. Before exposing caller SQL, its identifier
resolver must distinguish real table/field references from aliases and CTE names,
resolve them through the same map, and return logical labels/errors to callers.
This is not a promise that arbitrary token substitution is a SQL parser. Tests cover
two 64-character names sharing a 63-byte prefix, a short name equal to a long name's
first candidate, index/sequence collisions, and (when the query path lands) round-trip
query results and errors using logical names.

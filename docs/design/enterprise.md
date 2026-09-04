# Enterprise contract — design spec

**Local-first is the first invariant and it is non-negotiable: `auth: off` is the default and equals
today's v0.2.0 behavior byte-for-byte — no headers read, no principal, no `owner` column on default
tables, zero new steps to start.**

This document is the design authority for the #158 enterprise stream (authn, authz, row-level
access, hierarchy, `store.Engine` extraction, engine-2, dual-mode conformance, docs+skills). Every
decision below is already made; this doc pins it precisely so parallel streams never conflict.
Deviating from anything written here requires editing this spec **first, in the same PR** that
deviates. Terminology follows the vocabulary in §0 exactly.

---

## 0. Vocabulary

| Term | Meaning |
|---|---|
| **principal** | An opaque identity string asserted by a trusted proxy (or the bootstrap admin key). Dolmen never interprets its bytes; grants and `owner` values compare it exactly. |
| **group** | An opaque string naming a set of principals. Membership is never stored by dolmen — it arrives per-request from the trusted proxy (invariant 3: identity is consumed, never produced). |
| **trusted proxy** | A peer whose TCP source address falls inside a configured CIDR. Only trusted proxies may assert identity headers. |
| **verb** | One of `read`, `write`, `schema`, `admin` (§2). |
| **object** | A namespace path, a namespace/table pair, or the server root `*` (§3, §5). |
| **grant** | A durable (subject, object, verbs) tuple. Grants are additive; there are no deny grants. |
| **visible set** | For one request against one table: the rows the principal may observe. All rows when the table has no `row_access`; `owner = principal` otherwise, for callers without table-wide read (§4). |
| **engine** | An implementation of the `store.Engine` interface (§6). SQLite is adapter #1. |
| **mode** | `auth: off` (default, golden v0.2.0 contract) or `auth: on` (deny-by-default). |

## 1. Identity (authentication)

Dolmen terminates no user-facing authn. A gateway (Entra/GitHub via OAuth proxy, service-token
sidecar, …) authenticates the caller and forwards the asserted identity; dolmen consumes it.

### 1.1 Headers

| Header | Shape | Semantics |
|---|---|---|
| `X-Dolmen-Principal` | single value, UTF-8, 1–256 bytes after trim | The principal. Exact string; never interpreted, lowercased, or split. |
| `X-Dolmen-Groups` | comma-separated, each entry UTF-8 1–128 bytes after trim, no commas inside an entry, at most 32 entries | The principal's groups. Entries are trimmed; empty entries dropped; exact repeats deduplicated preserving order. |

*Why `X-Dolmen-*` and not `X-Forwarded-User`/`X-Forwarded-Groups`:* the `X-Forwarded-*` family is
comma-appendable by proxy chains and conventionally carries display-name/email semantics, so its
value shape is not ours to pin; a `X-Dolmen-` pair has exactly one producer and exact-match
semantics, and cannot be merged or rewritten in flight by generic forwarding layers.

### 1.2 Trusted-proxy configuration

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `-auth` | `DOLMEN_AUTH` | `off` | `off` = v0.2.0 behavior (§8). `on` = deny-by-default; identity is required. |
| `-trusted-proxies` | `DOLMEN_TRUSTED_PROXIES` | empty | Comma-separated CIDRs (bare IPs allowed). Only peers inside these ranges may assert §1.1 headers. |

Rules:

- Trust is decided from the **immediate TCP peer address** (`RemoteAddr`), never from
  `X-Forwarded-For` — *rationale: XFF chains are client-spoofable; the peer address is the one hop
  dolmen can verify without credentials.* Operators behind an L4/L7 balancer list the balancer CIDR.
- Headers from untrusted peers are ignored entirely (stripped before dispatch).
- With `auth: on`, a request with no resolvable identity — untrusted peer, missing
  `X-Dolmen-Principal`, malformed header values, over-limit groups — fails `401` with error code
  `unauthorized` (a new code; `auth: off` never emits it). Existing `forbidden` (403) means
  *authenticated but not granted*.
- `auth: on` with no identity source at all (no trusted proxies, no admin key) is a **startup
  error** — fail fast rather than a server that 401s everything.
- `/healthz`, `/version`, `/skills*`, and `/v1/openapi.json` remain unauthenticated in both modes
  (liveness probes and client-side schema discovery); everything under `/v1/{op}` and `/mcp`
  requires identity when `auth: on`.
- Audit attribution: with `auth: on` the principal is attached to the request's log line alongside
  the existing `X-Request-Id` correlation. Audit identity lives in logs, never in responses: dolmen
  adds no identity echo of its own to response bodies — the only principal strings a response can
  carry are row data (`owner`, already gated by the caller's visible set, §4.3) and grant records
  (`list_grants`, itself `admin`-gated).

### 1.3 Bootstrap admin key

| Environment variable | Shape |
|---|---|
| `DOLMEN_ADMIN_KEY` | base64url — `^[A-Za-z0-9_-]{32,256}$` (RFC 6750 Bearer `token68` without padding). Env-only, no flag twin. |

- Keys outside that alphabet are a **startup error**, not a warning: HTTP field parsing strips
  leading/trailing whitespace and clients/proxies may reject characters outside the Bearer
  credential grammar, so a broader charset would let a deployment pass its identity-source check
  with a credential that cannot be transmitted faithfully. Generate one with
  `openssl rand -base64 32 | tr '+/' '-_' | tr -d '='`.
- Presented as `Authorization: Bearer <key>`; compared in constant time.
- Maps to the built-in principal `dolmen-admin`, which implicitly holds `admin` on `*`. The implicit
  grant attaches to the **credential, not the name** — it is configuration, not data: never listed
  by `list_grants`, and removing the env removes the identity at next restart, while grants it
  minted persist normally.
- `dolmen-admin` is reserved in the header space: §1.1 headers asserting `X-Dolmen-Principal:
  dolmen-admin` are rejected (401, like malformed values), so no proxied identity can occupy the
  bootstrap principal and inherit its implicit grant.
- Accepted from any address (the key is a direct credential, not a proxy assertion).
- *Why env-only:* every non-secret flag has an env twin, secrets (`DOLMEN_EMBED_API_KEY`) do not —
  flags are visible in process listings.

Bootstrap flow: start with `DOLMEN_AUTH=on DOLMEN_ADMIN_KEY=…`, grant the first real principals
their verbs over `/v1/grant`, then drop the key from the environment.

## 2. Verbs

Exactly four, never extended without editing this spec:

| Verb | Grants | Notes |
|---|---|---|
| `read` | Row visibility | Table-wide on `row_access` tables (§4). Never grants any write or DDL. |
| `write` | Row mutation | On `row_access` tables also implies reading/mutating **own** rows through structured ops (§4). |
| `schema` | DDL + migration | Table structure and its history. |
| `admin` | Grants + namespace lifecycle | The only verb that can change what others may do, or delete a namespace. |

Operation → verb mapping (the complete op set; `grant`/`revoke`/`list_grants` are new ops defined
in §3):

| Operation | Verb required | Object checked |
|---|---|---|
| `list_namespaces` | none (any authenticated principal) | Response lists only namespaces the caller holds **any** grant on or under (§3.3). |
| `create_namespace` | `admin` | The **parent**: `*` for depth-1 namespaces, the containing namespace for deeper ones. Under `auth: on` this is the only way a namespace comes to exist (see below). |
| `drop_namespace` | `admin` | The namespace itself. Leaf-only (§5.4). |
| `list_tables` | none (any authenticated principal) | Authorization runs **before** the existence check: unless the caller holds any grant on or under the namespace, the response is `not_found` — indistinguishable from a nonexistent namespace, so listing cannot be used to enumerate names. Holders see the tables they hold any grant on. |
| `describe_table` | `read`, `write`, `schema`, **or** `admin` | The table. `row_count` follows the caller's visible set (§4.3) — holders of only `schema`/`admin` see the schema with a count of 0; no data visibility is implied. |
| `describe_server`, `infer_schema` | none (any authenticated principal) | Untargeted: provider status is no secret; `infer_schema` is pure computation. |
| `create_table` | `schema` | The namespace. |
| `drop_table`, `migrate`, `list_migrations` | `schema` | The table. Migration history is the audit trail of schema changes — same verb as the changes themselves. |
| `insert`, `upsert`, `upsert_by_key`, `update`, `delete` | `write` | The table. |
| `query` | `read` | The **namespace** — raw SQL may reference any table in it, so the grant must cover the namespace, not one table. See §4.4 for the extra rule on `row_access` tables. |
| `search_fulltext`, `search_vector` | `read` on the table; **or** `write` when the table declares `row_access` (search then covers own rows only, §4.3) | The table. |
| `grant`, `revoke` | `admin` | The target object (an ancestor grant suffices, §3.3). |
| `list_grants` | `admin` | The queried subtree (`*` when unfiltered). |

Deny-by-default: with `auth: on`, a request is authorized only by a matching grant (or the admin
key). Denials are `403 forbidden` with the same envelope as every other error. Authorization
*failure* (e.g. a corrupt grant store) is a `500 internal_error` — the check fails closed, never
open.

**Implicit namespace creation is disabled under `auth: on`.** v0.2.0 creates a namespace file on
first use, but that side effect would bypass the parent-level `admin` gate: a `schema` grant on a
not-yet-existing namespace must not materialize the namespace through `create_table` or any other
op. With `auth: on`, an operation targeting a nonexistent namespace fails `not_found` — **but only
after authorization succeeds**. Authorization precedes existence for every grant-protected op: an
ungranted caller receives `403 forbidden` whether or not the object exists (and `401` before
that, without identity), so no operation's error code — this one included — can be used to
enumerate namespaces or tables; `not_found` is visible only to callers authorized to know.
`create_namespace` is the only creation path. Under `auth: off` nothing changes — implicit
creation stays, exactly as v0.2.0.

Transport parity: `grant`/`revoke`/`list_grants` are ordinary ops — same envelope over `/v1/{op}`
and MCP `tools/call`. With `auth: off` they **do not exist**: not dispatchable, absent from
`tools/list` and `/v1/openapi.json`, keeping the auth-off surface byte-identical to v0.2.0.

## 3. Grant model

Authorization = embedded OpenFGA (SQLite tuple store, groups resolved as contextual tuples per
request). FGA is an implementation detail behind the three ops below; it never surfaces in the API.
Grants persist in a server-level registry SQLite file directly under the data directory (chosen so
its filename cannot match the namespace regex, e.g. a leading `_`), **above** the engine seam
(§6) — engines never see grants, only verb-gated calls and `RowScope`s.

### 3.1 Subjects and objects

```json
{"type": "principal" | "group", "id": "<opaque string, same limits as §1.1>"}
```

A principal matches subject `{"type":"principal","id":<their principal>}`; a group subject matches
any group in their `X-Dolmen-Groups`. Principals and groups never collide (the `type` discriminates).

Objects use the same shape as every other op — namespace path (§5), optional table:

```json
{"namespace": "acme"}                                  // the namespace AND everything under it
{"namespace": "acme/team-a", "table": "events"}        // one table
{"namespace": "*"}                                     // the whole server — the only wildcard
```

`*` is the only wildcard, and it is whole-object only. There are no segment wildcards
(`acme/*` is invalid) — *rationale: a grant on a namespace already inherits to every table and
sub-namespace inside it (§3.3), so segment wildcards would express nothing new while adding a
grammar to validate and interpolate safely.*

Grants may name objects that do not exist yet (grant-then-create is the natural order for
onboarding a team). Namespace paths and table names are validated by syntax only (§5.1). A table
requires a concrete namespace: `{"namespace": "*", "table": …}` is `invalid_request`.

### 3.2 Ops

```json
// POST /v1/grant
{"subject": {"type": "group", "id": "team-a"},
 "object": {"namespace": "acme/team-a"},
 "verbs": ["read", "write"]}

// response data
{"grant": {"subject": {"type": "group", "id": "team-a"},
           "object": {"namespace": "acme/team-a"},
           "verbs": ["read", "write"],
           "created_at": "2026-09-04T12:00:00.123Z"}}
```

- `verbs` is required, non-empty, duplicate-free; unknown verbs are `invalid_request`.
- Re-granting verbs an existing (subject, object) grant already holds is a no-op success returning
  the stored grant; new verbs are merged into it, keeping the original `created_at`. Grants are
  idempotent by (subject, object).
- `revoke` takes the same request shape (subject, object, verbs — explicit, no implicit "all");
  revoking verbs the grant does not hold is a no-op success. When the last verb is revoked the
  grant ceases to exist. Response: `{"grant": <remaining grant or null>}`.
- `list_grants` takes optional filters `object` (subtree: the named object and everything under it,
  aligned with grant inheritance) and `subject` (exact). Response:
  `{"grants": [<grant>, …]}` sorted by (subject.type, subject.id, object path).

### 3.3 Resolution

A principal's effective verbs on any object = the union over:

- every matching subject: their principal subject, plus one subject per group in the request;
- every covering object: `{"namespace":"*"}`, each ancestor namespace, the namespace itself, and
  the table itself.

Namespace grants inherit down to all tables and all sub-namespaces. `admin` on an object permits
`grant`/`revoke` on that object and everything under it (delegation follows the same direction as
inheritance). There are no deny grants, no precedence, no ordering — union only.

## 4. `row_access` and the implicit `owner` column

### 4.1 Declaration

`row_access` is a **table-level** annotation on `create_table` (fields stay untouched):

```json
{"namespace": "acme/team-a", "table": "notes",
 "fields": [{"name": "body", "type": "text"}],
 "row_access": "own"}
```

`"own"` is the only value in this stream; omitting the key means no row filtering (the default,
invariant 1). The key is accepted **only when `auth: on`** — with `auth: off` it is an unknown field
and rejected, exactly as v0.2.0 would.

The `owner` column materializes **only** when the table declares `row_access` — never on default
tables, in either mode (invariant 1; conformance-enforced, §8.3). `owner` is reserved exactly
where the implicit column exists: `create_table` with `row_access` rejects a caller-declared
field named `owner`, and `migrate set_row_access: true` likewise rejects when a caller-declared
`owner` field exists (the collision is named). Disabling `row_access` keeps the physical column
(§4.2), so the name stays reserved for as long as the schema carries it: `add_field`/`rename_field`
targeting `owner` is rejected on any table with the implicit column, enabled or disabled.
Everywhere else a caller field named `owner` remains valid — v0.2.0 does not reserve the name,
and reserving it unconditionally would break tables and requests that v0.2.0 accepts.

### 4.2 Column semantics

- Type: `TEXT`, nullable in DDL. With `auth: on` every row-insert path (`insert`, the insert branch
  of `upsert` and `upsert_by_key`) stamps the principal; callers cannot supply or set `owner` —
  same contract as `id` and `created_at`. NULL occurs only on rows written while `auth: off`
  (a table declared with `row_access` but filled from a local/off-mode writer).
- `owner` appears in row reads (`SELECT *`, search results) like `id`/`created_at` do, but never in
  the declared `fields` list of `create_table`/`describe_table` output.
- Enabling `row_access` later via `migrate` (`{"op": "set_row_access", "value": true}`) is rejected
  on a table with rows, with the error naming the row count — *rationale: ownership adopted after
  the fact has no honest backfill. No operation can write another principal's rows as that
  principal (inserts stamp the caller; `owner` is never caller-supplied), so the supported path is
  a fresh `row_access` table populated by replaying each owner's rows under their identity —
  directly or through the gateway — letting the server stamp every `owner` itself.*
  Disabling (`value: false`) is allowed: the column and its values remain, filtering stops.
- NULL-owner rows under `auth: on`: invisible to own-filtered callers; visible to table-wide
  readers (§4.3) — consistent with "no filter" being the stronger grant.

### 4.3 Enforcement: visible set → predicate

With `auth: on`, the API layer resolves verbs (§3.3), consults the table's `row_access`, computes
the request's **visible set**, and passes it to the engine as a `RowScope` (§6.3):

- Table without `row_access`, caller holding `read` or `write` → no scope (all rows).
- Table with `row_access` and a caller holding `read` through any covering grant → no scope
  (table-wide; the `read` verb is explicitly table-wide visibility).
- Table with `row_access` and a caller holding only `write` → scope `{owner = principal}`.
- Holders of only `schema` or `admin` have an **empty visible set on every table** — they hold no
  data verbs, so `row_access` is irrelevant to them: their one metadata path, `describe_table`,
  reports a count of 0 rather than the real table count. The API layer passes an explicit empty
  scope; a nil scope means *unscoped*, never *empty*.

**Scope resolution is incarnation-guarded** — the annotation is consulted above the seam, so a
`set_row_access` migration (or a drop-and-recreate, of the table **or the whole namespace**) must
not be able to race it. The API layer resolves the scope against the table's current
**incarnation** — the (namespace creation id, schema version, drop generation) triple, §6.3 — read
together with the schema from one snapshot (the engine's `TableState`, §6.2), and passes it
alongside the scope; the engine re-checks it inside the operation's transaction — the
same consistent-or-stale guard the store already applies to migrations and drops — and fails
`conflict` on a mismatch, after which the caller re-resolves and retries. The drop generation is
required because a version alone cannot distinguish a dropped table's same-named successor, which
is recreated at version 1; the namespace creation id is required because dropping a namespace
deletes its file and with it the drop generations, so a recreated namespace's tables would
otherwise repeat (version 1, generation 0). A default table replaced — by migration, table drop,
or namespace drop — by a `row_access` table of the same name can therefore never inherit a stale
nil scope, and a migration can never flip one into access to foreign rows.

The engine conjoins the scope predicate into every row read, count, mutation, and search it
performs for that call. Everything observes the visible set:

**Scoped filters are row-local.** The `filter` fragments accepted by `update`, `delete`, `upsert`,
and the searches are raw SQL WHERE expressions interpolated into the statement. Under a scope, a
subquery (`EXISTS (SELECT 1 FROM <target> WHERE owner <> ? AND secret = ?)`) or a cross-table
reference would turn returned counts into an oracle over invisible rows — the outer
`owner = ?` conjunct cannot protect against reads the filter itself performs. With `auth: on`,
filters are therefore restricted to **row-local expressions**: column references of the target
table, literals, bound `?` parameters, and scalar functions over them — no subqueries, no table
references (including `__fts` shadow tables), no aggregate or window functions. Violations are
`invalid_request`, validated above the seam before execution. `query` is unaffected: raw SQL
already requires namespace-wide `read` (§2), which authorizes every table its subqueries touch.
Under `auth: off` the filter language is unchanged from v0.2.0.

**The scope is a security barrier, not a sibling conjunct.** SQL does not guarantee conjunct
evaluation order, so `AND owner = ?` alone cannot make row-local expressions safe: a caller can
write `iif(secret = ?, abs(-9223372036854775808), 1)` — an integer-overflow *error* on a foreign
row reveals the foreign value even though the row can never affect the returned count. The engine
must therefore evaluate caller-supplied expressions only over the **already-filtered visible
set**: the scope predicate is applied first at a materialization boundary (a CTE/subquery stage,
or the engine's equivalent), and caller SQL — filters, set-expressions, search filters — never
executes against an invisible row, whatever the planner's chosen order.

**Migration responses disclose only visible-set data.** `migrate` executes table-wide (a `schema`
holder may restructure every row), and its *validations* cover every stored row — `set_enum` must
reject a vocabulary that any stored value falls outside, invisible or not. But its *disclosures*
follow the visible set: the plan's row counts (`backfill_rows`, `fulltext_reindex_rows`,
`embed_rows`) are reported for visible rows only, and `set_enum`'s rejection names the offending
values and their counts only to table-wide readers — a caller without table-wide read sees counts
of 0 and a generic rejection naming no values. *Rationale: today's `set_enum` error enumerates
every stored nonmember value; unscoped, a schema-only caller could harvest the distinct contents
of any string field by offering unlikely one-value vocabularies.*

- `update`/`delete` match only visible rows; `updated`/`deleted` counts are visible-set counts.
- `search_fulltext`/`search_vector` (and their `filter`, `min_score`, pagination, and `truncated`)
  are computed over the visible set — pagination can never surface or imply a foreign row.
- `describe_table`'s `row_count` is the visible-set count.
- `upsert_by_key`: natural-key matches against **invisible** rows are treated as no match — the
  insert branch adds a fresh row owned by the writer. Two rows sharing a natural key (one invisible
  to the writer) may coexist; that is the consistent outcome of visible-set semantics, and the
  collision never leaks existence. *Rationale: natural keys are expected to collide across users.*
- `insert` with an `idempotency_key`: the idempotency record stores the owner. A replay by the
  original owner (or any table-wide reader) returns the original ids verbatim. A replay by a
  principal who cannot see those rows gets `409 conflict` — never the ids, never a silent
  re-insert under the same key. *Rationale: idempotency keys are per-table unique; a foreign key
  replay is misuse, unlike a natural-key collision.*

### 4.4 Raw SQL

`query` (raw SQL) on a table that declares `row_access` requires **table-wide** `read` — i.e. a
`read` grant covering the table (§2 already requires `read` on the whole namespace). A
write-only caller's own-row reads go through the structured ops (`search_*`, `describe_table`,
their own writes' returned ids), never through `query`.

*Rationale: reliably conjoining `owner = ?` into arbitrary caller SQL (joins against the same table,
subqueries, aggregations) is not sound; raw SQL is the power tool and is gated accordingly.*

## 5. Namespace hierarchy grammar

### 5.1 Grammar

```
namespace-path := segment ("/" segment){0,2}          # depth 1–3
segment        := ^[a-z0-9][a-z0-9_-]{0,63}$          # unchanged from v0.2.0
table-object   := namespace-path "/" table-name        # table-name: existing rules unchanged
```

- No leading, trailing, or double slashes; no empty segments. Depth is capped at **3** namespace
  segments (`a/b/c`); a table ref adds one more but is not namespace depth.
- On direct `/v1` calls, namespace paths are trimmed and lowercased per segment before validation,
  exactly as v0.2.0 lowercases single names.

### 5.2 File layout

Namespace `a/b/c` is the SQLite file `<data>/a/b/c.db` (WAL sidecars alongside). The directory
`<data>/a/` (and `a/b/`) is created on first child creation. A namespace and its subtree may
coexist: `a` is `<data>/a.db`, its children live under `<data>/a/`. Depth-1 namespaces are exactly
v0.2.0's layout — no migration, no behavior change for existing data directories.

### 5.3 Listing

`list_namespaces` gains an optional `prefix` (a valid namespace path). With it: only namespaces
**under** that prefix, recursively. Without: all namespaces. Response shape is unchanged —
`{"namespaces": [...]}`, sorted lexicographically by full path (a depth-1-only store sorts
identically to v0.2.0). With `auth: on`, both forms list only the caller's visible namespaces
(§2). The `prefix` parameter is additive to the auth-off contract: v0.2.0 requests (no `prefix`)
behave identically (§8.1).

### 5.4 Recursive drop guard

`drop_namespace` drops exactly one namespace: its `.db` and sidecars, plus its registry — the
v0.2.0 semantics, unchanged. Dropping a namespace that **has descendants** is rejected
(`invalid_request`) with the error naming the descendant count; drop the children first.
*Rationale: no accidental tree deletion, and at depth ≤ 3 explicit child-first drops are cheap.*

## 6. `store.Engine` interface

The seam extracted by #76. Everything above it — envelope, error mapping, validation, authn/authz,
visible-set computation, skills/MCP/OpenAPI rendering — is engine-neutral and shared. SQLite becomes
adapter #1 with **zero contract change** (the conformance suite is the proof).

What stays above the seam: `infer_schema`, `describe_server` (pure/no engine), grant ops and the
grant store (server-level), identity and `RowScope` computation. What the engine owns: everything
namespace/table/row below.

### 6.1 Operation set

Namespace lifecycle, table DDL, row CRUD, filtered reads, migrate ops, search execution — mirroring
today's `*store.Store` methods.

### 6.2 Signature sketch

Sketch only — #76 owns the final signatures; the operation set and the `RowScope`/paging
conventions are what this spec pins:

```go
type Engine interface {
    // Namespace lifecycle — NamespaceState returns the namespace's creation
    // id (not_found when absent): the read the API uses to build CreateTable's
    // nsGen guard; an empty namespace has no TableState to consult.
    NamespaceState(ctx context.Context, ns string) ([16]byte, error)
    ListNamespaces(ctx context.Context, prefix string) ([]string, error)
    CreateNamespace(ctx context.Context, ns string) error
    DropNamespace(ctx context.Context, ns string) error

    // Table DDL and registry — DescribeTable's scope scopes the returned row
    // count to the caller's visible set (§4.3). The zero Incarnation (no
    // guard, auth off) is re-checked inside the operation: §4.3's scope guard.
    // TableState is the one-snapshot read the API layer resolves scopes with:
    // schema (row_access, vectorize field, embed-space identity) together with
    // the incarnation it must pass back — and it is also where a text vector
    // query validates its preconditions BEFORE the provider is called, so an
    // invalid query fails without contacting (or billing) the embedder.
    TableState(ctx context.Context, ns, table string) (*schema.TableSchema, Incarnation, error)
    ListTables(ctx context.Context, ns string) ([]string, error)
    // nsGen is the namespace's creation id: inside the operation's critical
    // section the engine verifies the namespace exists with EXACTLY that id —
    // table creation never creates a namespace implicitly and cannot race a
    // concurrent drop_namespace into recreating one past §2's parent-admin
    // gate. Zero = no guard (auth off).
    CreateTable(ctx context.Context, ns, table string, fields []schema.Field, opts TableOpts, nsGen [16]byte) (*schema.TableSchema, error)
    DescribeTable(ctx context.Context, ns, table string, scope *RowScope, scopeIncarnation Incarnation) (*schema.TableSchema, int64, error)
    DropTable(ctx context.Context, ns, table string) error

    // Migrate ops — emb re-embeds set_vectorize backfills. Dry runs are a
    // separate method returning the FULL MigrationPlan (operations, destructive
    // changes, backfill/reindex/embed row counts) — those row-dependent values
    // are computed against engine-owned data and cannot be rebuilt above the
    // seam. expected is the full Incarnation the plan was made against: a bare
    // version cannot distinguish a same-named successor recreated at version 1
    // (§4.3), so apply verifies the namespace id and drop generation too. The
    // zero value is the compatibility path, intentionally unguarded (auth off).
    PlanMigration(ctx context.Context, ns, table string, changes []schema.Change, emb Embedder, expected Incarnation) (*MigrationPlan, error)
    Migrate(ctx context.Context, ns, table string, changes []schema.Change, emb Embedder, expected Incarnation) (*schema.TableSchema, error)
    ListMigrations(ctx context.Context, ns, table string) ([]Migration, error)

    // Row CRUD — scope filters which existing rows may be matched, read, or counted;
    // on Insert it scopes the idempotency replay lookup (foreign-replay → conflict, §4.3).
    // WriteOpts carries the owner to stamp on EVERY row-insert path — including the
    // upsert insert branches, including table-wide callers whose scope is nil (§4.2) —
    // and insert's idempotency key. The stamp owner is independent of the scope:
    // a caller may be unscoped yet still be the writer. emb embeds vectorize fields
    // on write and re-embeds changed ones, passed per call exactly as today.
    Insert(ctx context.Context, ns, table string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)
    UpsertByKey(ctx context.Context, ns, table string, on []string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)
    Upsert(ctx context.Context, ns, table string, filter string, args []any, record map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)
    Update(ctx context.Context, ns, table string, filter string, args []any, set map[string]any, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (int64, error)
    // DeleteOpts carries the v0.2.0 safety guard (dry_run, limit, confirm) and the
    // engine enforces the threshold inside the delete transaction — an API-layer
    // preflight would race. DeleteResult keeps the contract's matched/deleted pair.
    Delete(ctx context.Context, ns, table string, filter string, args []any, opts DeleteOpts, scope *RowScope, scopeIncarnation Incarnation) (DeleteResult, error)

    // Filtered reads — Query takes NO scope: the API layer gates raw SQL by table-wide
    // read (§4.4), which is precisely why no scope parameter exists here.
    Query(ctx context.Context, ns, sql string, args []any, page Page) (QueryResult, error)

    // Search execution — includeHidden must cross the seam: truncated is
    // computed against the projected response-byte budget inside the engine,
    // so fetching hidden columns and stripping them above is not equivalent.
    // A text vector query validates against the TableState snapshot (vectorize
    // field present, identity pinned) and embeds BEFORE calling SearchVector —
    // preserving today's error precedence, where an invalid query never
    // reaches the embedding provider.
    SearchFulltext(ctx context.Context, ns, table, match string, filter string, args []any, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error)
    SearchVector(ctx context.Context, ns, table string, q VectorQuery, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error)

    Close() error
}
```

### 6.3 `RowScope`

```go
// RowScope restricts row visibility for one call. Nil = unscoped (auth off,
// or a table-wide reader). Non-nil = only rows with owner == Owner are visible,
// or no rows at all when Empty is set (schema/admin-only describe_table, §4.3).
type RowScope struct {
    Owner string
    Empty bool
}
```

```go
// Incarnation identifies one lifetime of a table. NsGen is the namespace's
// creation id — a random 128-bit value assigned when the namespace is
// created and persisted in its registry; DropNamespace deletes it with the
// file, so a recreated namespace gets a fresh one and the pair below can
// never repeat across namespace lifetimes (the drop generation alone cannot
// guarantee this: it lives inside the namespace database and is reset by the
// drop). Version+DropGen distinguish a dropped table's same-named successor,
// which is recreated at version 1 — the store already tracks drop
// generations (_dolmen_drop_gen) for exactly this. The zero value means
// "no guard" (auth off).
type Incarnation struct {
    NsGen   [16]byte
    Version int64
    DropGen int64
}
```

The engine knows nothing of principals, grants, or verbs; it receives an opaque owner string.
`WriteOpts` carries the stamp owner (absent under `auth: off`) and the idempotency key;
`DeleteOpts`/`DeleteResult` carry the delete guard and its `matched`/`deleted` pair. The
incarnation a scope was resolved against travels as the separate `scopeIncarnation` argument on
every scoped operation (§4.3's scope guard) — separate because the guard must bind even to a
**nil** scope: a request resolved before `row_access` was enabled, or against a predecessor table
of the same name, must not execute as unscoped afterwards. The existing `store.Embedder` injection
for vectorize paths is unchanged.

## 7. Search as contract

Results, ranking, and truncation are **engine-neutral contract**; the implementation (FTS5 index,
in-memory BM25 scan, ANN index) is engine-chosen. An index is an accelerator, never a semantic
change: adding, building, or dropping any index must not change results, ordering, `truncated`,
`skipped_vectors`, or `_score` beyond float tolerance. Shadow structures (FTS5 tables, ANN stores)
never appear in `list_tables` or any other surface.

What the contract pins (conformance-enforced on every engine):

| Property | Contract |
|---|---|
| Result shape | Rows as stored plus `id`/`created_at` (and `owner` when present), typed reads per field type; `_score` on every vector result (cosine, `-1..1`, engines agree within float tolerance); no rank value exposed for fulltext. |
| Ordering | `search_fulltext`: relevance descending, deterministic tiebreak `id` ascending. `search_vector`: `_score` descending, tiebreak `id` ascending. Identical corpus + query ⇒ identical order on every engine. Under a scope, ranking operates over the **visible corpus only**: relevance statistics must not include rows outside the caller's visible set — foreign matching rows can never reorder or displace visible results (§4.3). Predicate conjunction alone is not sufficient (a shared index's corpus statistics span owners); engines choose the isolation — per-scope index partitioning, or filter-then-rescore. |
| Match language | The documented FTS5 `MATCH` subset (terms, implicit AND, `OR`, `NOT`, `field:term`, `{a b}:term`, quoted phrases, `term*` prefix, `NEAR(...)`) with the documented tokenizer/stemmer behavior (porter over unicode61: case/diacritic folding, English stemming, opaque CJK runs). Engines must accept the whole subset; SQLite-only extensions to the grammar are not portable and not guaranteed. |
| Ranking quality | BM25-family relevance over the §7 match language; the conformance corpus fixes expected orderings, so "same suite passes" *is* the quality bar. |
| Truncated / pagination | `limit` default 10 max 200, `offset`, `truncated` exactly as v0.2.0 — always computed over the caller's visible set (§4.3). |
| Vector skips | Corrupt/dimension-mismatched/non-finite stored vectors are skipped and reported as `skipped_vectors`, never silently dropped. |

The SQL dialect of `filter`/`args` on searches and of `query` is SQLite SQL on adapter #1; engine-2's
dialect stance is engine-2's issue, and must keep the conformance corpus green.

## 8. Dual-mode conformance plan

The conformance suite (`internal/conformance`) runs in both modes in CI (`make test`); breaking
either mode fails CI.

### 8.1 What "byte-for-byte v0.2.0" means, precisely

The existing conformance package — every request and pinned response it contains — runs against an
`auth: off` server and passes **unmodified**: every request v0.2.0 accepts behaves identically, and
nothing v0.2.0 accepted may change meaning. Additive extensions are permitted — optional request
keys (`prefix`), newly valid inputs (deep namespace paths), new ops — and the schema documents may
grow accordingly (`list_namespaces` gaining `prefix`, the namespace pattern widening to allow `/`),
provided every schema change is strictly additive: each request that validated against the v0.2.0
schemas still validates and produces the same response. What never appears under `auth: off`:
`grant`/`revoke`/`list_grants` in dispatch, `tools/list`, or `/v1/openapi.json`, and the
`row_access` annotation in any schema.

### 8.2 Modes in the harness

- `auth: off` — today's harness, unchanged: full suite, golden contract.
- `auth: on` — a harness variant with `DOLMEN_AUTH=on`, `DOLMEN_TRUSTED_PROXIES=127.0.0.1/8`, and
  an admin key; tests assert identity via `X-Dolmen-Principal`/`X-Dolmen-Groups` (or the bearer key)
  the way a gateway would.

### 8.3 What auth:on adds to the suite

1. **Deny-by-default sweep** — for every op (all 22): no identity (401 `unauthorized`) and
   untrusted-peer identity (401). Authenticated-but-ungranted (403 `forbidden`) applies to the
   **grant-protected** ops only. The grant-free ops of §2 succeed for an ungranted authenticated
   caller (`describe_server`, `infer_schema`, `list_namespaces` — the latter returning an empty
   list) — except `list_tables`, which per §2 returns `not_found` for a caller holding no grant on
   or under the namespace; the sweep asserts exactly that. Envelope shapes pinned like every other
   error.
2. **The #158 acceptance scenario, end-to-end** — one test scripting the umbrella scenario:
   tables with default permissions; a user who writes but reads only their own rows; read-only
   access elsewhere; list/create in one place not another; raw SQL denied without table-wide read;
   `upsert_by_key` collision non-leak; idempotent replay owner-only; `truncated` never leaks
   foreign rows.
3. **Shared contract subset, re-run authorized** — envelope, error contract, typed reads, coercion,
   limits, search invariants, migration guards run under an authorized principal with identical
   expectations wherever the op is permitted (test tables parameterized by mode).
4. **Invariant tests** — `auth: off`: headers ignored even from trusted CIDRs (send them, assert
   no principal anywhere); no `owner` on default tables; the §8.1 rule holds — every v0.2.0-valid
   request still validates and responds identically, with schema changes additive-only.
   `auth: on`: default tables still have no `owner` (invariant 1's "never on default tables" holds
   in both modes); grant ops enforce §3; namespace hierarchy enforces §5.
5. **Fail-closed** — an authorization-check failure denies (500), never bypasses.

### 8.4 CI wiring

Both modes are ordinary `go test ./...` runs inside `make test` — mode selection happens inside the
harness per test group, no CI matrix, no new make targets.

---

## Decision index

| # | Decision | Where |
|---|---|---|
| D1 | `X-Dolmen-Principal` / `X-Dolmen-Groups` | §1.1 |
| D2 | Trust = immediate TCP peer in `-trusted-proxies`/`DOLMEN_TRUSTED_PROXIES` CIDRs; XFF never used for trust | §1.2 |
| D3 | `-auth`/`DOLMEN_AUTH`, default `off`; `auth: on` with no identity source fails startup | §1.2 |
| D4 | `DOLMEN_ADMIN_KEY` env-only bearer; principal `dolmen-admin`; implicit `admin` on `*` attaches to the credential, not the name; `dolmen-admin` reserved in headers | §1.3 |
| D5 | New error code `unauthorized` (401); `forbidden` (403) = granted-no | §1.2 |
| D6 | Verbs `read`/`write`/`schema`/`admin`; full op→verb table (`describe_table` also serves `schema`/`admin` holders, count 0); `query` gated on the namespace; `auth: on` disables implicit namespace creation (`not_found` instead) | §2 |
| D7 | Grants: subject {type,id}, object {namespace[,table] \| `*`}, explicit verbs; union resolution; inheritance down; no deny grants; no segment wildcards | §3 |
| D8 | Grant store is server-level, above the engine seam | §3 preamble |
| D9 | `row_access: "own"` table-level on create_table; auth-off rejects the key | §4.1 |
| D10 | `owner`: TEXT, server-stamped, materializes only on `row_access` tables; the name is reserved only there (v0.2.0 `owner` fields stay valid elsewhere); enabling later rejected on non-empty tables | §4.1–4.2 |
| D11 | Visible set → `RowScope` predicate passed to the engine; every count/mutation/search observes it | §4.3, §6.3 |
| D12 | Raw SQL on `row_access` tables requires table-wide read | §4.4 |
| D13 | Hierarchy: `/`-separated, v0.2.0 segment regex, depth ≤ 3, `<data>/a/b/c.db` layout, recursive-descendant `prefix` listing, leaf-only drops | §5 |
| D14 | Engine interface operation set + signature sketch; `Query` deliberately unscoped | §6 |
| D15 | Search semantics are contract; index vs scan is engine choice; index never changes semantics | §7 |
| D16 | Dual-mode conformance: precise byte-for-byte rule, deny sweep, acceptance scenario, invariant tests, no CI matrix | §8 |

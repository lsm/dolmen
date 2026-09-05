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
| **verb** | One of `create`, `read`, `update`, `delete`, `schema`, `admin` (§2). |
| **object** | A namespace path, a namespace/table pair, or the server root `*` (§3, §5). |
| **grant** | A durable (subject, object, verbs) tuple. Grants are additive; there are no deny grants. |
| **visible set** | For one request against one table: the rows the principal may observe. All rows when the table has no `row_access`; `owner = principal` otherwise, for callers without table-wide read (§4). |
| **engine** | An implementation of the `store.Engine` interface (§6). SQLite is adapter #1. |
| **mode** | `auth: off` (default, golden v0.2.0 contract) or `auth: on` (deny-by-default). |

## 0.5. Architecture invariants

### 0.5.1 Namespace is a logical tenant coordinate

Physical isolation is an **engine implementation detail**. Permissions bind to names — never to
files — so the tenancy model survives any engine swap. §5.2's `<data>/a/b/c.db` layout is
adapter #1's mapping, not the definition of tenancy:

| | SQLite (adapter 1) | Postgres (anticipated adapter 3) | DuckDB/Iceberg (adapter 2) |
|---|---|---|---|
| namespace → | one file | one schema | catalog namespace / storage prefix |
| isolation | physical (the file is the wall) | engine-enforced (schema confinement; optional native RLS) | engine-enforced (catalog scoping) |
| raw-SQL confinement | free (separate file) | role-per-schema **privileges** — the connection's role has no access to other schemas (`search_path` alone is only name resolution) | catalog/prefix ACLs — **privileges**, not qualification, are the wall |
| RowScope (own-rows) | predicate conjoined in SQL | predicate, **or delegated to native RLS** | predicate in the scan |

### 0.5.2 Two-level tenancy — two mechanisms, never mixed

- **Level 1, project isolation → namespaces (STRUCTURAL).** Hard wall; queries cannot cross;
  grants anchor here. The engine maps it to file / schema / catalog object. File-per-namespace is
  adapter #1's isolation *strategy* — the strongest cheap one — capped by fleet size (thousands
  of open handles), not correctness; when a fleet outgrows it, the Postgres adapter takes over
  with **nothing above the seam changing**.
- **Level 2, user isolation inside a project → rows (LOGICAL).** Users share the project's
  tables; each sees only their own rows (`owner` column + visible-set predicate, §4). Deliberately
  NOT structural: an `owner`-column WHERE is the lowest common denominator of every engine — that
  is what makes it portable — and engines with something better (Postgres RLS) may take over
  beneath the same contract.
- **User-per-namespace is an anti-pattern.** It explodes namespace count and makes shared tables
  (the usage-telemetry pattern, §8.3) impossible.
- **The architect's two knobs** (seed for the #167 skill docs): *structural* — give the team a
  sub-namespace (`acme/team-a`) when they must never see each other's schema or data; *logical* —
  one table with `row_access: "own"` when they share a table but keep private rows. The same
  grant language drives both.

### 0.5.3 Raw-SQL confinement is an engine obligation

Contract, not a SQLite accident: `query` executes within exactly ONE namespace, and the engine
must make cross-namespace reference **impossible by mechanism** — separate file (SQLite),
role-per-schema privileges (Postgres), catalog/prefix ACLs (lakehouse). Name-resolution pinning
(`search_path`, prefix qualification) is **not** confinement: a fully qualified
`other_schema.table` still resolves when the shared connection's role can reach it — the
mechanism must be privilege-based (a role with no access to other namespaces' objects) or an
equally strong check that rejects every cross-namespace reference; otherwise a namespace-level
`read` grant becomes cross-tenant access. This is why `query` gates on the namespace
(§2): per-table enforcement on arbitrary caller SQL is not sound; granularity lives in the
structured ops.

### 0.5.4 RowScope is engine-delegatable

The predicate is the contract; enforcement is dispatch checks (always dolmen, FGA) + row filtering
(engine-translated). Engines may delegate the filtering to native mechanisms (Postgres RLS) —
dolmen's check remains the contract guarantee, and the conformance suite is the proof it cannot
be skipped.

Forward note (non-normative): Postgres is the anticipated adapter #3 for operational SQL
workloads when file-per-tenant fleets cap out — the rationale for the engine-mapping invariant's
generality (file / schema / catalog), not a feature promise.

## 0.6. Consistency contract

Engine obligations, stated beside §0.5.3's confinement rule:

- **Atomicity.** Every operation is all-or-nothing; a failed operation leaves zero partial
  state, and an idempotency record commits atomically with its rows.
- **Ordering.** Writes to one namespace are observable as some serial order (engines may
  parallelize internally; the observable result must match one). Read-your-writes: once an
  operation acks, subsequent operations observe it. Reads never see torn batches.
- **Collision surfacing.** Concurrent-write anomalies are transparently serialized or retried
  internally within bounds; when neither is possible they surface as `409 conflict` — never
  partial writes, never corruption. Agent retry recipe (for the docs): on 409, re-read,
  re-issue.
- **Deployment topology is engine-declared.** `sqlite`: exactly one dolmen process per data
  directory (the existing README rule, now contract). Shared engines (`postgres`, `iceberg`):
  concurrent dolmen processes are safe via engine-native coordination. Declared so no one
  assumes HA an engine cannot give.

**Deliberate non-features** (recorded so they read as decisions, not omissions): **data
portability is SKIPPED** — no `export`/`import` ops; laptop→central promotion, per-tenant
offboarding, and engine-migration dumps all wait for a real-world demander (#32 stays open,
unrescoped; the seam keeps portability designed, not user-exercisable). **The audit surface is
DEFERRED** — no `list_audit` or audit-event store now; the audit story is §1.2 attribution
(principal on every op log line, with `X-Request-Id`) plus `list_migrations`; a structured audit
surface waits for a named compliance requirement. No features without demanders.

## 1. Identity (authentication)

Dolmen terminates no user-facing authn. A gateway (Entra/GitHub via OAuth proxy, service-token
sidecar, …) authenticates the caller and forwards the asserted identity; dolmen consumes it.

### 1.1 Headers

| Header | Shape | Semantics |
|---|---|---|
| `X-Dolmen-Principal` | single value, printable ASCII `^[!-~]{1,256}$` (0x21–0x7E: no space, no controls, no NUL/CR/LF, no non-ASCII) | The principal. Exact string; never interpreted, lowercased, or split. |
| `X-Dolmen-Groups` | comma-separated, each entry printable ASCII `^[!-~]{1,128}$` **excluding comma** (the separator), at most `-max-groups` entries (default 128, range 1–1024) | The principal's groups. Entries are trimmed; empty entries dropped; exact repeats deduplicated preserving order. Over-limit is a malformed identity → 401 (§1.2) — silently dropping a group that carries a grant would be worse. |

Both shapes are deliberately HTTP-header-safe: control characters and NUL are rejected by HTTP
parsers before dolmen can inspect the request, leading/trailing spaces are stripped by field
parsing, and non-ASCII bytes are obs-text that proxies may mangle — so none of them can appear in
an identity that a trusted proxy can faithfully assert. Values outside the charset are malformed
(→ 401, §1.2), and §3.1 grant subject ids carry the same charset (rejected `invalid_request`),
so no durable grant can name an unassertable identity.

*Why `X-Dolmen-*` and not `X-Forwarded-User`/`X-Forwarded-Groups`:* the `X-Forwarded-*` family is
comma-appendable by proxy chains and conventionally carries display-name/email semantics, so its
value shape is not ours to pin; a `X-Dolmen-` pair has exactly one producer and exact-match
semantics, and cannot be merged or rewritten in flight by generic forwarding layers.

### 1.2 Trusted-proxy configuration

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `-auth` | `DOLMEN_AUTH` | `off` | `off` = v0.2.0 behavior (§8). `on` = deny-by-default; identity is required. |
| `-trusted-proxies` | `DOLMEN_TRUSTED_PROXIES` | empty | Comma-separated CIDRs (bare IPs allowed). Only peers inside these ranges may assert §1.1 headers. |
| `-max-groups` | `DOLMEN_MAX_GROUPS` | `128` | Maximum group entries accepted per request (§1.1). Valid range 1–1024; values outside the range are rejected at startup, consistent with existing config validation. Non-secret, so flag + env twin per convention. *Why 128 and configurable (amended 2026-09-04): Entra tokens routinely carry 200+ group claims for well-connected users; 128 keeps default operations pain-free while the deployment guide tells gateway operators to filter to relevant groups.* |

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
  (liveness probes and client-side schema discovery; they expose no row data — **confirmed in
  review 2026-09-05, a decision, not a default**); everything under `/v1/{op}` and `/mcp`
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
- **Precedence when both mechanisms are present** (a trusted proxy commonly forwards the client's
  `Authorization` header alongside the identity headers): the direct bearer credential wins — a
  deliberate credential outranks an asserted header, and the proxy headers are ignored for that
  request. An **invalid** bearer value is never ignored in favor of a valid proxy identity: the
  request fails `401` (fail-closed — a rejected direct credential must not silently downgrade to
  the weaker mechanism).
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

Exactly six, CRUD-shaped, never extended without editing this spec:

| Verb | Grants | Notes |
|---|---|---|
| `create` | Append rows (`insert`) | On `row_access` tables implies own-row read through structured ops (§4.3). |
| `read` | Row visibility | Table-wide on `row_access` tables (§4). Never grants any write or DDL. |
| `update` | Mutate existing rows | Own rows only on `row_access` tables (§4.3). |
| `delete` | Remove rows | Own rows only on `row_access` tables (§4.3). |
| `schema` | DDL + migration | Table structure and its history. |
| `admin` | Grants + namespace lifecycle | The only verb that can change what others may do, or delete a namespace. |

*Why CRUD-shaped verbs (amended 2026-09-04 from design review; supersedes the four-verb
`read`/`write`/`schema`/`admin` set):* append-only tables must be expressible in grants — the
canonical shared **usage-telemetry** table lets every user append usage records while nobody,
row authors included, may update or delete; a bundled `write` cannot express that, and `create`
without `update`/`delete` expresses it exactly. Dolmen's ops are already CRUD-shaped
(`insert`/`update`/`delete` are distinct ops), so the mapping stays trivial. There are no role
presets or verb bundles in the API (`viewer`/`editor`/…); documentation may show canonical verb
bundles as examples only. `list` is not a verb — existence visibility follows grants via
authz-precedes-existence (§2), unchanged.

Operation → verb mapping (the complete op set; `grant`/`revoke`/`list_grants` are new ops defined
in §3):

| Operation | Verb required | Object checked |
|---|---|---|
| `list_namespaces` | none (any authenticated principal) | Response lists only namespaces the caller holds **any** grant on or under (§3.3). |
| `create_namespace` | `admin` | The **parent**: `*` for depth-1 namespaces, the containing namespace for deeper ones. Under `auth: on` this is the only way a namespace comes to exist (see below). |
| `drop_namespace` | `admin` | The namespace itself. Leaf-only (§5.4). |
| `list_tables` | none (any authenticated principal) | Authorization runs **before** the existence check: unless the caller holds any grant on or under the namespace, the response is `not_found` — indistinguishable from a nonexistent namespace, so listing cannot be used to enumerate names. Holders see the tables they hold any grant on. |
| `describe_table` | any verb (`read`, `create`, `update`, `delete`, `schema`, `admin`) | The table. `row_count` follows the caller's visible set (§4.3) — table-wide for `read`, own rows for the other data verbs on `row_access` tables, 0 for `schema`/`admin` holders; no data visibility beyond the caller's set is implied. |
| `describe_server`, `infer_schema` | none (any authenticated principal) | Untargeted: provider status is no secret; `infer_schema` is pure computation. |
| `create_table` | `schema` | The namespace. |
| `drop_table`, `migrate`, `list_migrations` | `schema`; `drop_table` additionally requires `admin` under `auth: on` — §3.4 makes a table drop delete every grant targeting the table, and changing what others may do is the `admin` verb, not `schema` | The table. Migration history is the audit trail of schema changes — same verb as the changes themselves. |
| `insert` | `create` | The table. |
| `update` | `update` | The table. |
| `delete` | `delete` | The table. |
| `upsert`, `upsert_by_key` | `create` **AND** `update` (both required) | The table. They are update-or-insert: a `create`-only caller is refused up front, not surprised by half the operation. |
| `query` | `read` | The **namespace** — raw SQL may reference any table in it, so the grant must cover the namespace, not one table. See §4.4 for the extra rule on `row_access` tables. |
| `search_fulltext`, `search_vector` | `read` on the table; **or** any data verb (`create`/`update`/`delete`) when the table declares `row_access` (search then covers own rows only, §4.3) | The table. |
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
Grants persist in a registry **above** the engine seam (§6) — engines never see grants, only
verb-gated calls and `RowScope`s — and the registry's placement is topology-bound (§0.6): in the
single-process topology (adapter #1) it is a server-level SQLite file directly under the data
directory (its filename chosen so it cannot match the namespace regex, e.g. a leading `_`); in a
multi-process shared-engine deployment, per-pod local grant stores are **forbidden** — replicas
would authorize differently and a revoke acked by one pod would stay live on another. Grants live
in a deployment-wide coordinated backend (the shared engine itself, or the deployment runs a
single authorization process); grant changes are visible to every replica with §0.6's ordering
guarantees.

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

Grants target **existing objects only** (`{"namespace":"*"}` excepted as the root): no
pre-provisioning grants against nonexistent namespaces — create, then grant (§3.4). Namespace
paths and table names are validated by existence, not just syntax (§5.1). A table
requires a concrete namespace: `{"namespace": "*", "table": …}` is `invalid_request`.

### 3.2 Ops

```json
// POST /v1/grant
{"subject": {"type": "group", "id": "team-a"},
 "object": {"namespace": "acme/team-a"},
 "verbs": ["create", "read", "update", "delete"]}

// response data
{"grant": {"subject": {"type": "group", "id": "team-a"},
           "object": {"namespace": "acme/team-a"},
           "verbs": ["create", "read", "update", "delete"],
           "created_at": "2026-09-04T12:00:00.123Z"}}
```

- `verbs` is required, non-empty, duplicate-free; unknown verbs are `invalid_request`. Requests
  may list verbs in any order, but `grant.verbs` in every response (`grant`, `revoke`,
  `list_grants`) is serialized in the fixed §2 order — `create`, `read`, `update`, `delete`,
  `schema`, `admin` — so the same durable grant never serializes differently across re-grants,
  restarts, or listings.
- Re-granting verbs an existing (subject, object) grant already holds is a no-op success returning
  the stored grant; new verbs are merged into it, keeping the original `created_at`. Grants are
  idempotent by (subject, object).
- `revoke` takes the same request shape (subject, object, verbs — explicit, no implicit "all");
  revoking verbs the grant does not hold is a no-op success. When the last verb is revoked the
  grant ceases to exist. Response: `{"grant": <remaining grant or null>}`.
- `list_grants` takes optional filters `object` (subtree: the named object and everything under it,
  aligned with grant inheritance) and `subject` (exact). Response:
  `{"grants": [<grant>, …]}` sorted by (subject.type, subject.id, namespace path, then table
  name — a namespace-only grant sorts before grants on tables inside it; sorting by a flat
  "object path" string would tie `acme/team` (namespace) against `acme`'s table `team`, leaving
  the order implementation-defined).

### 3.3 Resolution

A principal's effective verbs on any object = the union over:

- every matching subject: their principal subject, plus one subject per group in the request;
- every covering object: `{"namespace":"*"}`, each ancestor namespace, the namespace itself, and
  the table itself.

Namespace grants inherit down to all tables and all sub-namespaces. `admin` on an object permits
`grant`/`revoke` on that object and everything under it (delegation follows the same direction as
inheritance). There are no deny grants, no precedence, no ordering — union only.

### 3.4 Grant lifecycle

- **No ownership concept — deliberately.** Creators need none: `create_namespace` requires
  `admin` on the parent, and grants inherit down (§3.3), so the creator of a namespace always
  already administers it; delegation composes via targeted child grants (grant `admin` on
  `acme/team-a` specifically). Recorded so no one adds an owner concept later.
- **Last-admin lockout guard.** Revoking the final grant of `admin` on `*` is refused — a
  `409 conflict`-family error with a teaching message. This makes the bootstrap flow's advice to
  drop `DOLMEN_ADMIN_KEY` after the first grants permanently safe. No guard below `*`: an
  admin-less namespace still has ancestor admins.
- **Drop cascades grant deletion — crash-atomically.** Dropping a namespace or table deletes the
  grants targeting that object and its subtree; recreation starts with a clean grant slate — a
  reused namespace name cannot inherit the previous tenant's grants (offboarded contractors, the
  resurrection risk). Ancestor grants survive and apply, which is correct because an
  ancestor-admin does the recreating. The drop's `confirm` flow reports the number of grants that
  die with the object. Because the grant registry lives above the seam from the engine's
  deletion, the cascade is coordinated by a **write-ahead tombstone**: the tombstone is recorded
  in the grant registry FIRST and immediately excludes the subtree's grants from evaluation
  (they deny, never bypass); the engine deletion then runs; the grant rows are physically removed
  on completion. At recovery, a pending tombstone is finalized if the engine object is gone and
  rolled back (restoring evaluation) if the object still exists — either crash point converges to
  no resurrectable grants and no silently-granted successors, satisfying §0.6's all-or-nothing
  guarantee. The tombstone records the deleted object's **Incarnation**, and recovery finalizes
  whenever THAT incarnation is gone — even if a same-named successor already exists (a crash
  after the engine deletion but before cleanup, with a concurrent recreate, would otherwise roll
  the tombstone back on "name exists" and restore the predecessor's grants onto the successor).
  The tombstone also captures the **exact grant rows** it covers as a fixed set at creation:
  exclusion-from-evaluation and physical removal apply to exactly that set and nothing else —
  grants minted after tombstone creation (which, per §3.4's existing-objects rule and the
  incarnation guards, can only target the successor) are never denied or deleted by the
  predecessor's cleanup.
- **Grants target existing objects only** (`*` excepted as the root): no pre-provisioning grants
  against nonexistent namespaces — create, then grant.
- **Grant rows bind to their target's LIFETIME identity, recorded at grant time** (possible
  because grants target existing objects): a table grant records (`NsGen`, `Table`, `DropGen`) —
  the schema `Version` is deliberately excluded, exactly as idempotency records exclude it, so a
  migration never silently revokes direct table grants; a namespace grant records the namespace's
  `nsGen`; an ancestor grant records the **ancestor's** `nsGen`; a `*` grant records nothing. A
  targeted grant authorizes only against the lifetime it names — a predecessor's grant can never
  authorize a successor, at any hop, including the bootstrap state read (§6.2: the binding handed
  to `NamespaceState` comes from the authorization layer's matched grant row, never from the
  caller; an inherited grant verifies its ANCESTOR's incarnation there while receiving the
  TARGET's current generation — inheritance bootstraps descendants). An ancestor-admin
  administering a recreated namespace is by design. Grant mutations targeting a subtree with a
  **pending tombstone** fail `409` until cleanup completes, so no grant can slip between the
  tombstone's captured set and the engine deletion.

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
  on a table with rows — *rationale: ownership adopted after the fact has no honest backfill. No
  operation can write another principal's rows as that principal (inserts stamp the caller;
  `owner` is never caller-supplied), so the supported path is a fresh `row_access` table populated
  by replaying each owner's rows under their identity — directly or through the gateway — letting
  the server stamp every `owner` itself.* **The table-wide-`read` requirement is enforced BEFORE
  the populated-table test** (§4.3's data-dependent-migrations rule: callers without it are
  `403` before any row-dependent check runs) — so the populated rejection, including its row
  count, is only ever seen by table-wide readers, who are authorized to know; a schema-only
  caller learns nothing, not even whether the table has rows (a generic non-reader rejection
  would still distinguish empty from populated).
  Disabling (`value: false`) is allowed — the column and its values remain, filtering stops. It
  is not a feature but the **recovery hatch** for a mistaken declaration: without it, undoing
  `row_access` requires dropping and recreating the table — data loss. It **additionally requires
  table-wide `read` (or `admin`)**: removing the filter widens every
  data-verb holder's visibility from own rows to all rows (§4.3), so leaving it on `schema` alone
  would let a `schema`+`update` caller unscope themselves and then mutate every owner's rows — a
  direct escalation.
- NULL-owner rows under `auth: on`: invisible to own-filtered callers; visible to table-wide
  readers (§4.3) — consistent with "no filter" being the stronger grant.

### 4.3 Enforcement: visible set → predicate

With `auth: on`, the API layer resolves verbs (§3.3), consults the table's `row_access`, computes
the request's **visible set**, and passes it to the engine as a `RowScope` (§6.3):

- Table without `row_access`, caller holding `read` or any data verb (`create`/`update`/`delete`)
  → no scope (all rows).
- Table with `row_access` and a caller holding `read` through any covering grant → no scope
  (table-wide; the `read` verb is explicitly table-wide visibility).
- Table with `row_access` and a caller holding any data verb (`create`/`update`/`delete`) but not
  `read` → scope `{owner = principal}` — own-row visibility rides with ANY data verb: a
  `create`-only caller may search the rows they appended.
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
table, literals, bound `?` parameters, and functions drawn from an **enumerated engine-neutral
allowlist** — operators `||`, arithmetic, comparison, `AND`/`OR`/`NOT`, `IS`/`IS NOT`, `IN`
(literal lists), `BETWEEN`, `LIKE`, and `CASE`; functions `abs`, `round(x[,n])`, `length`,
`lower`, `upper`, `substr(x,y[,n])`, `trim`, `ltrim`, `rtrim`, `replace`, `instr`, `coalesce`,
`ifnull`, `nullif`, `iif`, and `date`, `time`, `datetime`, `julianday`, `strftime` — the
date/time functions accept only **explicit time values and deterministic modifiers**: `'now'`,
`'localtime'`, `'utc'`, and every other wall-clock or host-timezone-dependent form is
`invalid_request`; a caller wanting a now-relative comparison computes the timestamp and binds
it as a `?` parameter — **this list is the whole allowlist**, not a
category sketch: no subqueries, no table references (including `__fts` shadow tables), no
aggregate or window functions, and no other function of any kind — user-defined, data-reading,
nondeterministic (`random`, …), `sqlite_*` internals, or side-effecting ones are all
`invalid_request`. **Evaluation semantics are SQLite's, pinned**: `LIKE` is ASCII-case-insensitive
(true: `'A' LIKE 'a'`), string comparison uses BINARY byte-wise collation, integer division
truncates toward zero, `round` rounds half away from zero, NULL follows three-valued logic, and
coercion follows SQLite's documented rules — engines whose native functions differ (Postgres
`LIKE` is case-sensitive; collations vary) implement SQLite's semantics for scoped filters:
one shared evaluator, not just a shared validator. Materializing the visible rows first
constrains a function's *arguments* but never SQL executed inside its body, so an impure
function's results or errors would still be an oracle over hidden data. Validation happens above
the seam before execution, from this list — one shared rule, not per-adapter judgment. `query` is unaffected: raw SQL already requires
namespace-wide `read` (§2), which authorizes every table its subqueries touch. Under `auth: off`
the filter language is unchanged from v0.2.0.

**The scope is a security barrier, not a sibling conjunct.** SQL does not guarantee conjunct
evaluation order, so `AND owner = ?` alone cannot make row-local expressions safe: a caller can
write `iif(secret = ?, abs(-9223372036854775808), 1)` — an integer-overflow *error* on a foreign
row reveals the foreign value even though the row can never affect the returned count. The engine
must therefore evaluate caller-supplied expressions only over the **already-filtered visible
set**: the scope predicate is applied first at a materialization boundary (a CTE/subquery stage,
or the engine's equivalent), and caller SQL — filters, set-expressions, search filters — never
executes against an invisible row, whatever the planner's chosen order.

**Migration responses disclose only visible-set data.** `migrate` executes table-wide (a `schema`
holder may restructure every row), and its *disclosures* follow the visible set: the plan's row
counts (`backfill_rows`, `fulltext_reindex_rows`, `embed_rows`) are reported for visible rows
only, and `set_enum`'s rejection names the offending values and their counts only to table-wide
readers — a caller without table-wide read sees counts of 0 and a generic rejection naming no
values. *Rationale: today's `set_enum` error enumerates every stored nonmember value; unscoped, a
schema-only caller could harvest the distinct contents of any string field by offering unlikely
one-value vocabularies.*

**Data-dependent migrations require table-wide read.** Redacting the message is not enough: the
validation's *outcome* is itself a disclosure, and some migrations *process* hidden rows. A
caller without table-wide read could repeatedly attempt `set_enum` with chosen vocabularies and
distinguish success from the generic rejection — a repeatable membership oracle over invisible
values (and constraints can be cleared afterwards, so the oracle is practically exploitable).
Under `auth: on`, a migration whose outcome or execution depends on rows outside the caller's
visible set additionally requires table-wide `read` on the table; callers without it are denied
`403` before any data-dependent check or processing runs. The list: `set_enum` (value
membership), `set_row_access` enabling (row existence), `add_field` of a required field without a
backfill default (row existence), and **`set_vectorize` enabling or re-enabling — unconditionally,
even on an empty table** — its backfill reads every non-empty value and sends it to the embedding
provider, exporting invisible rows at the caller's direction, and the provider's outcome itself
depends on those hidden inputs; conditioning the gate on the table having rows would itself be a
row-existence oracle (succeeds on empty, 403 on invisible population). **`add_field` of a
`vectorize: true` field with a backfill `default` is on the list too** — apply embeds the default
for every existing row, so provider calls, cost, timing, and failure all read on hidden row
population. **Every FTS-rebuilding path is on the list as well** — `set_fulltext` enabling or
re-asserting `true` (which triggers a rebuild) and any other change that rebuilds an existing
full-text index: the rebuild scans every indexed value, so its latency, resource use, and
failures depend on hidden row count and content — a repeatable oracle even with
`fulltext_reindex_rows` redacted. And **every `add_field` carrying a backfill `default`** — not
only vectorized ones: apply runs `UPDATE` over every existing row, so the work, latency, and
storage impact read on hidden population, and the caller can re-probe by dropping and re-adding
fields. So does **every vector-clearing path** — `set_vectorize` disabling and dropping the
vectorized field: apply nulls `_embedding` across every row, so runtime, I/O, and failures read
on hidden population. So does **every `drop_field`** — even an ordinary non-FTS, non-vector
column drop rewrites storage across every hidden row, and add-nullable-then-drop cycles make the
probe repeatable. All other migrations are
data-independent and stay on the `schema` verb alone.

- `update`/`delete` match only visible rows; `updated`/`deleted` counts are visible-set counts.
- `search_fulltext`/`search_vector` (and their `filter`, `min_score`, pagination, and `truncated`)
  are computed over the visible set — pagination can never surface or imply a foreign row.
- `describe_table`'s `row_count` is the visible-set count.
- `upsert_by_key`: natural-key matches against **invisible** rows are treated as no match — the
  insert branch adds a fresh row owned by the writer. Two rows sharing a natural key (one invisible
  to the writer) may coexist; that is the consistent outcome of visible-set semantics, and the
  collision never leaks existence. *Rationale: natural keys are expected to collide across users.*
- `insert` with an `idempotency_key`: the idempotency record stores the owner. A replay returns
  the original ids verbatim to the **original owner, or any caller holding table-wide `read`**;
  any other caller gets `409 conflict` — never the ids, never a silent re-insert under the same
  key. (The engine distinguishes them via `WriteOpts.TableWideRead`, §6.2 — a nil scope alone
  cannot, because a `create`-only caller on a default table and a table-wide reader are both
  unscoped.) The foreign-collision `409` is decided **before any payload comparison**: an
  unauthorized replayer receives `409` regardless of whether the submitted payload matches the
  recorded one — returning the hash-mismatch `invalid_request` for wrong-payload guesses would
  let the caller distinguish correct from incorrect payload guesses and oracle the hidden
  payload. *Rationale: idempotency keys are per-table unique; a foreign key replay is misuse,
  unlike a natural-key collision.* Idempotency records die with their table incarnation: dropping
  a table removes its records atomically with it (adapter #1's behavior), or the engine binds
  the record to the **table-lifetime components only — `NsGen`, `Table`, `DropGen`** — in the
  lookup. The schema `Version` is deliberately excluded: it increments on every migration, and
  binding to it would miss a pre-migration record after any `migrate` and re-insert the retried
  payload instead of replaying the original ids — idempotency must survive migrations. Either
  way, a same-named successor table must never replay a predecessor's ids.

### 4.4 Raw SQL

`query` (raw SQL) on a table that declares `row_access` requires **table-wide** `read` — i.e. a
`read` grant covering the table (§2 already requires `read` on the whole namespace). A caller
without table-wide `read` reaches their own rows through the structured ops (`search_*`,
`describe_table`, their own writes' returned ids), never through `query`.

*Rationale: reliably conjoining `owner = ?` into arbitrary caller SQL (joins against the same table,
subqueries, aggregations) is not sound; raw SQL is the power tool and is gated accordingly. The
namespace-level confinement itself is an engine obligation, impossible by mechanism — §0.5.3.*

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

This layout is **adapter #1's mapping** of the namespace coordinate (§0.5.1), not the definition
of tenancy. Namespace `a/b/c` is the SQLite file `<data>/a/b/c.db` (WAL sidecars alongside). The directory
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
v0.2.0 semantics, unchanged — **and cascades grant deletion** (§3.4): the grants targeting the
namespace and its subtree die with it, the `confirm` flow reports that count, and recreation
starts with a clean grant slate. Dropping a namespace that **has descendants** is rejected
(`invalid_request`) with the error naming the descendant count; drop the children first.
*Rationale: no accidental tree deletion, and at depth ≤ 3 explicit child-first drops are cheap.*

## 6. `store.Engine` interface

The seam extracted by #76 — and, with §0.5, the **tenancy portability guarantee**: everything the
contract says about namespaces, isolation, and row visibility holds on every engine. Everything
above it — envelope, error mapping, validation, authn/authz,
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
    //
    // GLOBAL RULE: the engine NEVER creates a namespace implicitly. Every
    // operation that opens a namespace — listing, table state, DDL, rows,
    // search — requires it to exist, checked atomically with the operation
    // (not_found otherwise); adapter #1's create-on-open path (Store.ns) is a
    // v0.2.0 behavior that ends at the seam. And EVERY engine call whose
    // authorization was resolved against an object carries and atomically
    // verifies that object's incarnation — nsGen for namespace-level calls
    // (Query and the namespace lifecycle/listing methods below), the full
    // Incarnation for table-level calls — so a drop-and-recreate between
    // authorization and execution can never act on a successor the old grant
    // does not cover. Zero values = no guard (auth off).
    // Bootstrap: the AuthBinding handed to NamespaceState is WHAT THE
    // AUTHORIZATION LAYER PROVED, never caller-supplied (§3.4): a targeted
    // grant's TargetGen mismatches a successor; an inherited grant verifies
    // its ANCESTOR's (path, nsGen) while the call returns the TARGET's
    // current generation — inheritance bootstraps descendants; a Root (*)
    // grant has nothing to verify. The zero value = no guard (auth off).
    //
    // type AuthBinding struct {
    //     Root        bool       // matched a * grant
    //     Ancestor    string     // ancestor namespace path, when inherited
    //     AncestorGen [16]byte   // that ancestor's nsGen at grant time
    //     TargetGen   [16]byte   // target's nsGen, for targeted grants
    // }
    NamespaceState(ctx context.Context, ns string, auth AuthBinding) ([16]byte, error)
    ListNamespaces(ctx context.Context, prefix string) ([]string, error)
    // parentNsGen binds child creation to the authorized parent: creating
    // acme/team-a was authorized against acme's incarnation, and the engine
    // verifies that incarnation atomically with creation — a drop/recreate of
    // the parent between authorization and execution cannot place the child
    // under a successor. Zero = no parent guard (depth-1 child of `*`, or
    // auth off).
    CreateNamespace(ctx context.Context, ns string, parentNsGen [16]byte) error
    DropNamespace(ctx context.Context, ns string, nsGen [16]byte) error

    // Table DDL and registry — DescribeTable's scope scopes the returned row
    // count to the caller's visible set (§4.3). The zero Incarnation (no
    // guard, auth off) is re-checked inside the operation: §4.3's scope guard.
    // TableState is the one-snapshot read the API layer resolves scopes with:
    // schema (row_access, vectorize field, embed-space identity) together with
    // the incarnation it must pass back — and it is also where a text vector
    // query validates its preconditions BEFORE the provider is called, so an
    // invalid query fails without contacting (or billing) the embedder.
    // TableState is itself authorization-bound: nsGen is the NAMESPACE
    // incarnation the caller's grant check was resolved against (from
    // NamespaceState), verified atomically with the read — otherwise a
    // stale grant could fetch the SUCCESSOR's incarnation here and pass it
    // to a later scoped operation, laundering expired authorization through
    // the very call that mints the guard. Zero = no guard (auth off).
    TableState(ctx context.Context, ns, table string, nsGen [16]byte) (*schema.TableSchema, Incarnation, error)
    ListTables(ctx context.Context, ns string, nsGen [16]byte) ([]string, error)
    // nsGen is the namespace's creation id: inside the operation's critical
    // section the engine verifies the namespace exists with EXACTLY that id —
    // table creation never creates a namespace implicitly and cannot race a
    // concurrent drop_namespace into recreating one past §2's parent-admin
    // gate. Zero = no guard (auth off).
    CreateTable(ctx context.Context, ns, table string, fields []schema.Field, opts TableOpts, nsGen [16]byte) (*schema.TableSchema, error)
    DescribeTable(ctx context.Context, ns, table string, scope *RowScope, scopeIncarnation Incarnation) (*schema.TableSchema, int64, error)
    DropTable(ctx context.Context, ns, table string, inc Incarnation) error

    // Migrate ops — emb re-embeds set_vectorize backfills. Dry runs are a
    // separate method returning the FULL MigrationPlan (operations, destructive
    // changes, backfill/reindex/embed row counts) — those row-dependent values
    // are computed against engine-owned data and cannot be rebuilt above the
    // seam. expected is the full Incarnation the plan was made against: a bare
    // version cannot distinguish a same-named successor recreated at version 1
    // (§4.3), so apply verifies the namespace id and drop generation too. The
    // zero value is the compatibility path, intentionally unguarded (auth off).
    // scope on PlanMigration is the DISCLOSURE scope (§4.3): the plan's row
    // counts are computed over the caller's visible set, while validation
    // remains table-wide. Nil = unscoped (auth off, or a table-wide reader).
    // scopeIncarnation is the guard that scope was resolved against — verified
    // inside the operation exactly as for every other scoped operation; the
    // optional `expected` migration precondition CANNOT serve as this guard
    // (a dry run may carry no precondition), and a stale nil scope applied to
    // a recreated row_access successor would expose its counts. Zero = no
    // guard (auth off).
    // The plan also carries the Incarnation it was planned against, surfaced
    // in the dry-run response as an opaque `expected_incarnation` token; the
    // public migrate request accepts it and apply rejects on mismatch — the
    // plan→apply binding survives across requests, so a table dropped and
    // recreated between dry run and apply can no longer accept a stale
    // destructive plan. `expected_version` alone remains the auth-off
    // compatibility path: under `auth: on`, an apply carrying a precondition
    // MUST carry `expected_incarnation` — a version-only precondition is
    // `invalid_request`, because version 1 cannot identify a same-named
    // predecessor and the version-only path would re-open exactly the
    // destructive race the token closes.
    PlanMigration(ctx context.Context, ns, table string, changes []schema.Change, emb Embedder, expected Incarnation, scope *RowScope, scopeIncarnation Incarnation) (*MigrationPlan, error)
    Migrate(ctx context.Context, ns, table string, changes []schema.Change, emb Embedder, expected Incarnation) (*schema.TableSchema, error)
    ListMigrations(ctx context.Context, ns, table string, inc Incarnation) ([]Migration, error)

    // Row CRUD — scope filters which existing rows may be matched, read, or counted;
    // on Insert it scopes the idempotency replay lookup (foreign-replay → conflict, §4.3).
    // WriteOpts carries the owner to stamp on EVERY row-insert path — including the
    // upsert insert branches, including table-wide callers whose scope is nil (§4.2) —
    // insert's idempotency key, and TableWideRead: set iff the caller holds `read`
    // through a covering grant. The idempotency replay decision needs it because nil
    // scope alone cannot distinguish a create-only caller on a default table from a
    // table-wide reader (§4.3): replay returns ids iff the record's owner equals the
    // stamp owner OR TableWideRead, else conflict. The stamp owner is independent of
    // the scope: a caller may be unscoped yet still be the writer. emb embeds
    // vectorize fields on write and re-embeds changed ones, passed per call as today.
    Insert(ctx context.Context, ns, table string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)
    UpsertByKey(ctx context.Context, ns, table string, on []string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)
    Upsert(ctx context.Context, ns, table string, filter string, args []any, record map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error)
    Update(ctx context.Context, ns, table string, filter string, args []any, set map[string]any, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (int64, error)
    // DeleteOpts carries the v0.2.0 safety guard (dry_run, limit, confirm) and the
    // engine enforces the threshold inside the delete transaction — an API-layer
    // preflight would race. DeleteResult keeps the contract's matched/deleted pair.
    Delete(ctx context.Context, ns, table string, filter string, args []any, opts DeleteOpts, scope *RowScope, scopeIncarnation Incarnation) (DeleteResult, error)

    // Filtered reads — Query takes NO scope: the API layer gates raw SQL by table-wide
    // read (§4.4), which is precisely why no scope parameter exists here. nsGen is the
    // namespace lifecycle guard: the namespace-level read authorization was resolved
    // against THAT namespace incarnation, and the engine verifies it atomically with
    // execution — a drop-and-recreate between authorization and execution cannot let a
    // predecessor's grant read a successor tenant's data. Zero = no guard (auth off).
    Query(ctx context.Context, ns, sql string, args []any, nsGen [16]byte, page Page) (QueryResult, error)

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
// generations (_dolmen_drop_gen) for exactly this. Table binds the token to
// its table: two same-namespace tables both at (version 1, DropGen 0) share
// NsGen and would otherwise compare equal, so a plan's token could be
// replayed against a different table — the engine verifies Table matches the
// request's table. The zero value means "no guard" (auth off).
type Incarnation struct {
    NsGen   [16]byte
    Table   string
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
`skipped_vectors`, or `_score` **at all** — every reported score is exactly the mode's value
(raw, auth: off; canonical, auth: on, per the table below), with no tolerance window: a permitted
tiny drift could cross a bucket boundary or alter the serialized score, changing an otherwise
identical response. Shadow structures (FTS5 tables, ANN stores)
never appear in `list_tables` or any other surface.

What the contract pins (conformance-enforced on every engine):

| Property | Contract |
|---|---|
| Result shape | Rows as stored plus `id`/`created_at` (and `owner` when present), typed reads per field type; `_score` on every vector result (cosine; `-1..1` is the **auth-on clamped guarantee** — under `auth: off` the raw value is preserved and may marginally exceed the range, per Ordering); no rank value exposed for fulltext. |
| Ordering | **Two tiers.** Under `auth: off`, adapter #1's v0.2.0 behavior is preserved bit-for-bit (§8.1): raw cosine — unclamped, so a self-comparison may report `1.0000000000000002` — exact comparisons, and full-text ordered exactly as v0.2.0 executes it, `ORDER BY rank, rowid` (the explicit `rowid` tiebreak, not "native tie order"); nothing in this row redefines that mode, and engine-2 passes the golden auth-off corpus by reproducing adapter #1's exact arithmetic and ordering (which the canonical accumulation below already matches). With `auth: on`, and for cross-engine conformance generally: `search_fulltext` orders by **`q(rank)` ASCENDING** — FTS5 rank is lower/more-negative for more relevant rows, so ascending buckets ARE relevance-descending — tiebreak `id` ascending; `search_vector` orders by `q(_score)` descending, tiebreak `id` ascending — via **canonical arithmetic, then transitive quantization**. Canonical cosine, fully specified: BOTH operands are normalized to float32 first — the query
vector (raw caller-supplied or provider-embedded) is rounded to nearest float32 exactly as
adapter #1 does today, matching the stored float32 vectors — then computed in binary64; dot products and squared norms accumulated component-wise in dimension order; every multiply and add individually rounded (IEEE-754 round-to-nearest-even) — fused multiply-add/contraction and reassociation are FORBIDDEN; norms via the correctly-rounded square root; if either vector has zero norm the score is exactly `0` (never NaN, never skipped); the final quotient clamped to `[-1, 1]`. Engines may use faster internal paths only if the canonical value (and hence its bucket) is identical; the conformance corpus verifies. Quantize the canonical value with prescribed arithmetic: `q(s) = floor(s / fl64(1e-9))` — the divisor is the binary64 value nearest `1e-9`, the division is one correctly-rounded binary64 operation, then `floor` — computed in binary64 by EVERY engine regardless of internal storage (a decimal-backed engine evaluating the division exactly would put `q(0.5)` in bucket 500000000 where the prescribed binary64 division yields 499999999; the binary64 result is the contract). Order by the integer `q(_score)` (ties break by `id` ascending); `min_score` is constrained to the cosine range `[-1, 1]` (`invalid_request` outside), and the threshold compares `q(s) ≥ q(min_score)`. Quantizing per-engine approximations would NOT be consistent, and pairwise epsilon is non-transitive; canonical-then-bucket avoids both. Identical corpus + query ⇒ identical order on every engine. Under a scope, ranking operates over the **visible corpus only**: relevance statistics must not include rows outside the caller's visible set — foreign matching rows can never reorder or displace visible results (§4.3). Predicate conjunction alone is not sufficient (a shared index's corpus statistics span owners); engines choose the isolation — per-scope index partitioning, or filter-then-rescore. |
| Match language | **Two tiers** (amended 2026-09-05). **Core subset — contract on EVERY engine**: terms, implicit AND, `OR`, `NOT`, quoted phrases, `term*` prefix — identical semantics across engines, conformance-enforced, with the documented tokenizer/stemmer behavior (porter over unicode61: case/diacritic folding, English stemming, opaque CJK runs). **Extended grammar — engine-documented, not guaranteed portable**: `field:term`, `{a b}:term`, `NEAR(...)`; SQLite (adapter #1) supports all of it natively, other engines may implement any of it, and engines document which extended constructs they accept — unsupported extended syntax fails with the same teaching-error quality as everything else. *Rationale: requiring a scan-and-score engine to reimplement the full FTS5 grammar is lift without a demander; the core subset covers observed agent usage.* |
| Ranking quality | The normative full-text ranking is SQLite FTS5's built-in BM25 (`rank`) with default parameters — k1=1.2, b=0.75, all column weights 1.0 — computed over the visible corpus. "BM25-family" is not a license for a variant: differing idf definitions, length normalization, parameters, or field weights reorder the same corpus while still feeling like BM25. Two tiers, mirroring Ordering: under `auth: off`, adapter #1's explicit `ORDER BY rank, rowid` is preserved bit-for-bit per §8.1. With `auth: on`, the normative rank values are adapter #1's emitted ranks, bit-for-bit — SQLite FTS5 as the reference oracle, other engines reproduce its output exactly (corpus-verified on fixtures) — and near-equal scores compare with the same canonical quantization as vectors, `q(rank) = floor(rank / fl64(1e-9))` with `id` tiebreak, so precision, evaluation order, and log rounding cannot reorder documents or shift offset pages between engines. |
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
   foreign rows. Plus the append-only telemetry scenario (§2's amendment rationale): a shared
   `create`-only telemetry table where user A appends a row; A's `update`/`delete` on their own
   row is 403; A still sees their own rows via search; user B cannot see A's rows; a
   `create`-only caller's `upsert_by_key` is refused (needs `update` too); the developer
   (`read`+`schema`) reads all rows.
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
| D1 | `X-Dolmen-Principal` / `X-Dolmen-Groups`; groups cap configurable via `-max-groups`/`DOLMEN_MAX_GROUPS` — default 128, range 1–1024, startup-rejected outside; over-limit stays 401 | §1.1–1.2 |
| D2 | Trust = immediate TCP peer in `-trusted-proxies`/`DOLMEN_TRUSTED_PROXIES` CIDRs; XFF never used for trust | §1.2 |
| D3 | `-auth`/`DOLMEN_AUTH`, default `off`; `auth: on` with no identity source fails startup | §1.2 |
| D4 | `DOLMEN_ADMIN_KEY` env-only bearer; principal `dolmen-admin`; implicit `admin` on `*` attaches to the credential, not the name; `dolmen-admin` reserved in headers | §1.3 |
| D5 | New error code `unauthorized` (401); `forbidden` (403) = granted-no | §1.2 |
| D6 | Verbs `create`/`read`/`update`/`delete`/`schema`/`admin` (CRUD-shaped, amended 2026-09-04 from the four-verb set — append-only tables must be expressible); full op→verb table (`upsert`/`upsert_by_key` need `create` AND `update`; own-row visibility rides with any data verb; `describe_table` serves any verb, count per visible set); `query` gated on the namespace; `auth: on` disables implicit namespace creation (`not_found` instead) | §2 |
| D7 | Grants: subject {type,id}, object {namespace[,table] \| `*`}, explicit verbs; union resolution; inheritance down; no deny grants; no segment wildcards | §3 |
| D8 | Grant store is server-level, above the engine seam | §3 preamble |
| D9 | `row_access: "own"` table-level on create_table; auth-off rejects the key | §4.1 |
| D10 | `owner`: TEXT, server-stamped, materializes only on `row_access` tables; the name is reserved only there (v0.2.0 `owner` fields stay valid elsewhere); enabling later rejected on non-empty tables | §4.1–4.2 |
| D11 | Visible set → `RowScope` predicate passed to the engine; every count/mutation/search observes it | §4.3, §6.3 |
| D12 | Raw SQL on `row_access` tables requires table-wide read | §4.4 |
| D13 | Hierarchy: `/`-separated, v0.2.0 segment regex, depth ≤ 3, `<data>/a/b/c.db` layout, recursive-descendant `prefix` listing, leaf-only drops | §5 |
| D14 | Engine interface operation set + signature sketch; `Query` deliberately unscoped | §6 |
| D15 | Search semantics are contract; index vs scan is engine choice; index never changes semantics; match language is tiered — core subset (terms, implicit AND, OR, NOT, phrases, prefix) contract on every engine, extended grammar (`field:term`, `{a b}:term`, `NEAR`) engine-documented | §7 |
| D16 | Dual-mode conformance: precise byte-for-byte rule, deny sweep, acceptance scenario, invariant tests, no CI matrix | §8 |
| D17 | Architecture invariants: namespace is a logical tenant coordinate (permissions bind to names, never files); two-level tenancy — projects structural via namespaces (engine-mapped file/schema/catalog), users logical via the owner predicate (delegatable to native RLS); user-per-namespace is an anti-pattern; raw-SQL confinement is an engine obligation; Postgres anticipated as adapter #3 | §0.5 |
| D18 | Grant lifecycle: no owner concept (inheritance covers creators); last-admin-on-`*` revoke guard (409); drop cascades grant deletion (clean slate on recreation, count reported in drop confirm); grants target existing objects only | §3.4 |
| D19 | Consistency contract: op atomicity; per-namespace serial observability + read-your-writes; 409-or-nothing collision surfacing; engine-declared deployment topology (sqlite = 1 process/data-dir; shared engines = N pods) | §0.6 |
| D20 | Data portability SKIPPED and audit surface DEFERRED, deliberately — no features without real-world demanders (#32 open, unrescoped) | §0.6 |

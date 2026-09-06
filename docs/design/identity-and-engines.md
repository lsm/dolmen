# Identity, tenancy, and engines — design spec

**Local-first is the first invariant and it is non-negotiable: `auth: off` is the default and equals
today's v0.2.0 behavior byte-for-byte — no headers read, no principal, no `owner` column on default
tables, zero new steps to start.**

This document is the design authority for the #159 epic — auth, multi-tenancy, and pluggable
engines (authn, authz, row-level access, hierarchy, `store.Engine` extraction, engine-2,
dual-mode conformance, docs+skills) — serving every deployment size from local single-user to
organization-scale gateway deployments. Every
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

| | SQLite (adapter 1) | Postgres (adapter 2 — the reference shared engine) |
|---|---|---|
| namespace → | one file | one schema |
| isolation | physical (the file is the wall) | engine-enforced (schema confinement; native RLS available) |
| raw-SQL confinement | free (separate file) | role-per-schema **privileges** — the connection's role has no access to other schemas (`search_path` alone is only name resolution) |
| RowScope (own-rows) | predicate conjoined in SQL | predicate, **or delegated to native RLS** |

**Postgres is the reference shared engine (amended 2026-09-06):** dolmen is an *operational*
store — governed row-level multi-user CRUD, synchronous typed reads, idempotency — and only an
operational engine can honor that contract. The seam's invariants (schema-per-namespace, native
RLS for RowScope, role-per-schema confinement, ACID) are Postgres's native vocabulary; it is the
only engine that can honor the contract at shared scale today. File-per-tenant fleets that cap
out graduate to Postgres — the org/multi-host tier.

**The lakehouse is read-side, not an engine (amended 2026-09-06):** Iceberg/Delta (and
DuckDB-as-engine) are *analytical* — no cheap point updates, no fine-grained row grants, no
FTS5 — so they cannot honor the write contract and are reclassified out of the engine-adapter
list. Dolmen's data may be **published** to Parquet/Iceberg/Delta via a background export/CDC
path so Spark/Trino/DuckDB can query it alongside the operational store: **designed-not-built,
demander-gated (D20), the same status as webhooks (§9)** — a bolt-on read path, NOT a
`store.Engine` implementation. This is DISTINCT from the user-facing `export`/`import` ops D20
skips: those are synchronous portability ops and stay skipped; this is a deferred background
publish path, and the seam keeps it designed-not-user-exercisable.

### 0.5.2 Two-level tenancy — two mechanisms, never mixed

- **Level 1, project isolation → namespaces (STRUCTURAL).** Hard wall; queries cannot cross;
  grants anchor here. The engine maps it to a file or a schema object. File-per-namespace is
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
- **The architect's two knobs** (seed for the docs+skills stream's skill docs): *structural* — give the team a
  sub-namespace (`acme/team-a`) when they must never see each other's schema or data; *logical* —
  one table with `row_access: "own"` when they share a table but keep private rows. The same
  grant language drives both.

### 0.5.3 Raw-SQL confinement is an engine obligation

Contract, not a SQLite accident: `query` executes within exactly ONE namespace, and the engine
must make cross-namespace reference **impossible by mechanism** — separate file (SQLite),
role-per-schema privileges (Postgres). Name-resolution pinning
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

Forward note (non-normative): the engine-mapping invariant stays general (file / schema) even
though exactly two operational engines are on the list — SQLite for the local tier, Postgres
(adapter #2, the reference shared engine, above) for the org/multi-host tier; the lakehouse tier
is read-side publish, not an engine.

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
  directory (the existing README rule, now contract). Shared engines (`postgres`):
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

The contract of this section is mode-level: under `auth: on`, a request resolves to exactly one
`(principal, groups)` pair or fails `401` (§1.2). **Production of that pair is a set of additive,
pluggable identity sources behind one seam** (amended 2026-09-06): enable any mix; a request is
authenticated when any enabled source yields a principal; everything downstream of the seam —
verbs (§2), grants (§3), RowScope and owner stamping (§4) — is **source-blind** and unchanged by
which source fired. `auth: off` = no sources enabled, byte-identical v0.2.0 — permanently (§8.1).

| Source | Config | Credential | Notes |
|---|---|---|---|
| A — trusted-proxy headers (v1) | `-trusted-proxies` CIDRs (§1.2) | asserted headers (gateway session) | unchanged (D1/D2) |
| B — native OIDC (designed; built on demand) | `DOLMEN_AUTH_OIDC_ISSUER` / `_CLIENT_ID` / `_CLIENT_SECRET` (+ optional scopes; optional GitHub preset) | stateless signed token (Ed25519, default 7–14 d TTL, configurable) | §1.4 |
| C — API keys | `create_key` / `list_keys` / `revoke_key` ops (§1.5) | `dlm_…` bearer, stored hashed | §1.5 |
| D — admin key (bootstrap) | `DOLMEN_ADMIN_KEY` env (§1.3) | bearer → `dolmen-admin` | unchanged (D4) |

Source A is the v1 default: dolmen terminates no user-facing authn itself — a gateway
(Entra/GitHub via OAuth proxy, service-token sidecar, …) authenticates the caller and forwards the
asserted identity (§1.1–1.2). Source B is the one exception where dolmen runs an authentication
protocol directly (§1.4).

Precedence with several sources enabled: a deliberate bearer credential outranks an asserted
header (§1.3); among bearer sources, shape selects — a `dlm_` prefix routes to the key registry,
structural token separators route to signed-token verification, otherwise the admin-key compare
runs — and a bearer that fails the interpretation its shape selects is `401`, never silently
reinterpreted as a weaker source (fail-closed, §1.3). The shapes stay disjoint by construction:
§1.3 rejects an admin key beginning with `dlm_` at startup, so no credential is ever two
interpretations at once and dispatch never needs a second guess.

Build order (mirrored on the epic's authn stream, #159's checklist): the authn stream builds
**the seam itself — identity
sources as a pluggable interface, with the header source as v1**. Native OIDC and API keys land
later as one additive stream: they touch only the source layer and the key registry, never FGA.

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
| `-max-subscription-age` | `DOLMEN_MAX_SUBSCRIPTION_AGE` | `30m` | Ceiling on `subscribe` connection age (§9.3): at the bound the server teaching-closes and the client reconnects — re-asserting headers, hence refreshing a source-A identity — resuming from its cursor. Valid range `0` or `1s`–`24h`; other values rejected at startup, consistent with existing config validation. `0` disables the bound, documented as removing source A's identity-refresh backstop — not recommended with the header source enabled. Non-secret, so flag + env twin per convention. *Why 30m (added 2026-09-06): the bound is the backstop for the gateway's connection-termination obligation (§9.3) — short enough that a gateway identity change takes effect within minutes-to-an-hour, long enough not to churn healthy streams.* |

Rules:

- Trust is decided from the **immediate TCP peer address** (`RemoteAddr`), never from
  `X-Forwarded-For` — *rationale: XFF chains are client-spoofable; the peer address is the one hop
  dolmen can verify without credentials.* Operators behind an L4/L7 balancer list the balancer CIDR.
- Headers from untrusted peers are ignored entirely (stripped before dispatch).
- With `auth: on`, a request with no resolvable identity — untrusted peer, missing
  `X-Dolmen-Principal`, malformed header values, over-limit groups — fails `401` with error code
  `unauthorized` (a new code; `auth: off` never emits it). Existing `forbidden` (403) means
  *authenticated but not granted*.
- `auth: on` with no identity source at all (no trusted proxies, no admin key, no OIDC source,
  and no active API key) is a **startup error** — fail fast rather than a server that 401s
  everything. An API-key-only instance is a valid mix (§1): after bootstrapping an active key
  whose principal holds a durable root grant, the deployment may drop every other source. So is
  `auth: on` with **no usable root administrator**: neither the admin key nor a durable grant of
  `admin` on `*` targeting a **principal**. Group grants do not count — membership is asserted per
  request and never stored (§0), so startup cannot establish that the group has any member, and an
  empty or retired external group would silently satisfy the check while no identity can actually
  administer — **one decidable exception**: the API-key source stores its keys' groups (§1.5),
  so a root group grant **counts when an active key's stored groups locally prove membership** —
  the machine-group mechanism can then serve as the sole root administrator of an API-key-only
  deployment, and revoking the proving key or that group grant joins the same cross-registry
  lock as every other root mutation (§1.5, §3.4). A grant naming the reserved `dolmen-admin`
  does not count either — no source can
  yield that principal (§1.3), so such a grant is permanently unusable. A trusted-proxy CIDR
  alone leaves every proxied identity authenticated-but-ungranted, and `grant` itself requires
  `admin` — nobody could create the first grant without a restart. And the two predicates must
  not be satisfiable by disjoint identities — the durable root grant's principal must be
  **reachable through an enabled source**. Reachability is decidable for exactly one source: the
  API-key registry is local, so an API-key-only deployment requires at least one **active** key
  bearing the root principal — bob's active key satisfying the source check while alice's grant
  satisfies the administrator check boots a server nobody can administer, a startup error. For
  the header and OIDC sources reachability is **assumed, not verified** — an explicit, documented
  assumption: dolmen cannot observe which principals a gateway will assert or which subjects an
  IdP will still authenticate (verifying would mean probing the identity provider per principal —
  deliberately not built), so a principal root grant alongside either enabled source is accepted
  as reachable, and retiring the gateway account or IdP subject that holds root admin is an
  operator error this check cannot catch — with **one decidable exception**: source B's
  principals are issuer-qualified (§1.4), so when reachability is being established through the
  OIDC source, a root grant whose principal carries a *different*
  issuer's qualification than the currently configured `DOLMEN_AUTH_OIDC_ISSUER` is **not
  reachable through that source** — the new issuer can never yield it, the mismatch is visible
  locally without probing the provider. Reachability is an **OR across enabled sources**: if the
  header source is enabled (any well-formed principal is assertable) or an active API key bears
  exactly that principal, the grant is reachable through that source and the deployment starts
  — the issuer comparison governs only the OIDC branch, never the whole check. Within the OIDC
  branch, a stale qualification fails the usable-root-administrator check naming the stale
  grant rather than succeeding with an administrator who can never log in. Every reachability
  lockout, the rest included, has one
  universal recovery: set `DOLMEN_ADMIN_KEY` and restart — the kingmaker re-enters (§1.3) while
  the grants persist; the guards around root administration exist to make accidental lockout
  hard, not operator error unrecoverable. The same check guards the
  other end: removing `DOLMEN_ADMIN_KEY` from the environment is only safe once a durable
  principal root-admin grant exists (the §3.4 last-admin guard then keeps it un-revocable).
- `/healthz`, `/version`, `/skills*`, and `/v1/openapi.json` remain unauthenticated in both modes
  (liveness probes and client-side schema discovery; they expose no row data — **confirmed in
  review 2026-09-05, a decision, not a default**); everything under `/v1/{op}` and `/mcp`
  requires identity when `auth: on`. `/v1/auth/begin` and its callback (§1.4) are unauthenticated
  under `auth: on` by construction — they *are* the authentication — and do not exist under
  `auth: off`.
- Audit attribution: with `auth: on` the principal is attached to the request's log line alongside
  the existing `X-Request-Id` correlation. Audit identity lives in logs, never in responses: dolmen
  adds no identity echo of its own to response bodies — the only principal strings a response can
  carry are row data (`owner`, already gated by the caller's visible set, §4.3) and grant records
  (`list_grants`, itself `admin`-gated).

### 1.3 Bootstrap admin key

| Environment variable | Shape |
|---|---|
| `DOLMEN_ADMIN_KEY` | base64url — `^[A-Za-z0-9_-]{32,256}$` (RFC 6750 Bearer `token68` without padding). Env-only, no flag twin. |

**The bootstrap deadlock, written out (amended 2026-09-06).** With `auth: on` every op needs a
grant, and `grant` itself needs an `admin` grant — a fresh server therefore deadlocks: nobody can
create the first grant. `DOLMEN_ADMIN_KEY` breaks it.

- Keys outside that alphabet are a **startup error**, not a warning: HTTP field parsing strips
  leading/trailing whitespace and clients/proxies may reject characters outside the Bearer
  credential grammar, so a broader charset would let a deployment pass its identity-source check
  with a credential that cannot be transmitted faithfully. A key beginning with the reserved
  `dlm_` prefix is likewise a **startup error**: bearer dispatch routes that shape exclusively to
  the API-key registry and never retries the admin-key compare (§1 preamble), so a `dlm_`-prefixed
  admin key would pass validation yet never authenticate — locking the deployment out of bootstrap
  administration. Generate one with
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
  minted persist normally. The key holder is thus **a kingmaker, not a king**: its purpose is to
  mint the first real administrators, not to reign — power that lives only as long as the
  credential does, and everything it confers survives as ordinary grants.
- `dolmen-admin` is reserved across **all** sources (amended 2026-09-06): §1.1 headers asserting
  `X-Dolmen-Principal: dolmen-admin` are rejected (401, like malformed values), and no key or
  OIDC principal may occupy it (§1.4–1.5) — so no minted identity can inherit the bootstrap
  principal's implicit grant. The bootstrap identity exists only while its credential does.
- Accepted from any address (the key is a direct credential, not a proxy assertion).
- *Why env-only:* every non-secret flag has an env twin, secrets (`DOLMEN_EMBED_API_KEY`) do not —
  flags are visible in process listings.

Bootstrap flow: start with `DOLMEN_AUTH=on DOLMEN_ADMIN_KEY=…`, grant the first real principals
their verbs over `/v1/grant`, then remove the key from the environment and restart — the identity
vanishes while its grants persist. The removal is permanently safe: §1.2's startup check requires
a usable root administrator (the key **or** a durable `admin` on `*` grant), and the last-admin
guard (§3.4) then keeps that root grant un-revocable.

### 1.4 Native OIDC (source B — designed, not built)

Motivation: the gateway-less small-team tier — everything needed to run behind an IdP without
standing up a proxy. PocketBase-shaped by intent: the provider abstraction is endpoints + scopes +
one normalize-to-`AuthUser` method, ~500–800 lines total, and the expensive 90% of PocketBase
auth — user records, email flows, session machinery — is deliberately skipped (§1.6). Generic
OIDC covers Entra, Okta, Google, etc.

- Config: `DOLMEN_AUTH_OIDC_ISSUER`, `DOLMEN_AUTH_OIDC_CLIENT_ID`, `DOLMEN_AUTH_OIDC_CLIENT_SECRET`
  (the secret env-only per the §1.3 convention), plus optional extra scopes and an optional GitHub
  preset that fills in endpoints and claim mapping.
- Flow: `/v1/auth/begin` → the IdP's authorization endpoint → callback with PKCE and state/CSRF →
  code exchange. `/v1/auth/begin` and the callback are **the one deliberate non-JSON browser
  surface** — a tiny page that hands the token out — with the same exceptional status as `/mcp`;
  every op keeps the JSON envelope. Both are `auth: on`-only and unauthenticated by construction
  (§1.2).
- The credential is a **stateless signed token**: Ed25519-signed, default TTL in the 7–14 d
  range, configurable, presented as a bearer. No session store — the trade-offs are on record in
  §1.6. The **signing key is persistent, deployment-wide configuration, never per-process**:
  single-process deployments persist it beside the grant registry; shared-engine multi-process
  topologies coordinate it exactly as the grant registry's placement is topology-bound (§3
  preamble, §0.6) — a token issued by one replica must verify on every other, and a restart
  must not invalidate live tokens. Rotation is keyring-style: mint the successor, verify both
  during the overlap, retire the predecessor — the operational form of §1.6's revoke-all-humans.
- **Principal = the `sub` claim, never the email** — grants survive email changes; email is
  display-only and not stored (§1.6). Groups come from claims. Caveat, documented rather than
  papered over: Entra emits group claims as object GUIDs, not names, so Entra deployments either
  sync names or grant on the GUIDs. And because OIDC subject identifiers are unique **only
  within an issuer**, source B's principals and groups are **issuer-qualified**: the source
  yields the pair (issuer, `sub`) under a **stable, injective, grant-safe encoding** — the
  issuer folds to a fixed-length base32 digest (collision risk negligible and pinned), the
  `sub` (or group claim) appends verbatim, and the whole is validated against §3.1's subject
  charset and length at authentication: an identity that cannot be encoded within the grant
  limits is rejected at the source (`401`) rather than authenticating into a body no durable
  grant can name. Deterministic and injective across issuers by construction, so the same
  (issuer, claim) always yields the same principal and different issuers never collide; the
  exact serialization is otherwise the source layer's. The same encoding qualifies group claims.
  A deployment that changes
  `DOLMEN_AUTH_OIDC_ISSUER` therefore mints a disjoint principal population: a same-`sub` user
  at the new issuer is a *different* principal and inherits nothing — grants from the old
  issuer never match (fail-closed), and if the old issuer's principals held the only root
  grants, §1.2's usable-root-administrator check names that at startup rather than letting a
  stranger in silently.
- Built on demand; until then it exists as this design. The `dolmen-admin` reservation applies
  (§1.3).

### 1.5 API keys (source C)

Machines are principals too, but a machine cannot do an OAuth dance, and a shared long-lived user
token is the wrong shape for a CI job. API keys are minted, named, individually revocable
credentials:

- Ops: `create_key` / `list_keys` / `revoke_key` — `admin` verb, object checked `*` (§2):
  a key can bear *any* principal and optional groups (so group grants work for machines), and
  minting an identity that did not exist is administrative at the root, above any one namespace —
  the grants such an identity can use still have to be granted separately.
- Shape: `dlm_…` bearer, shown in full exactly once at creation; `list_keys` returns names,
  principals, and key state (active/revoked) — never credentials. The uniform `401` below is the
  **caller-side** surface only; admins see key state through listing.
- Stored **hashed** in the server-level registry beside grants (§3) — a registry leak does not
  leak credentials. Lookup and comparison follow §1.3's constant-time convention. Rejection of a
  revoked or unknown key is a plain `401` (§1.2) — indistinguishable, like every other auth
  failure.
- A key may not bear the reserved principal `dolmen-admin` (§1.3): the bootstrap identity exists
  only while its credential does, and a minted key would outlive it.
- **Self-revocation guard.** `revoke_key` mirrors §3.4's last-admin rule at the credential layer,
  and the protected invariant is **deployment-wide, not per-principal**: a revocation is a `409`
  (teaching message) only when it would leave the deployment with **no usable root administrator
  reachable through any enabled source** — not when it merely removes the caller's own last key.
  In an API-key-only deployment where alice and bob each hold a usable root grant and one active
  key, alice revoking her key is allowed — bob remains reachable; where alice is the only
  reachable root administrator, her last key is refused: the durable grant would survive while no
  credential could authenticate as it, wedging the running server and failing the next startup's
  reachability check. A per-principal reading would make keys less than individually revocable
  and force administrators to remove the associated grant first — the deployment-wide invariant
  keeps the credential layer self-contained. The
  check is an **atomic invariant over the deployment-wide set of active keys bearing usable-root
  principals or proving usable-root group grants (§1.2's key-provable exception), serialized in
  the key registry itself** (the same
  topology-bound coordination that orders grant mutations, §3): with one root principal and two
  active keys, two concurrent `revoke_key` calls on different replicas cannot each observe the
  other key still active and both commit — the registry's serialization admits one, and the
  second fails `409`. A single-request wording alone would not hold in the multi-replica
  topology. And the invariant spans **both registry regions under one lock**: keys live beside
  grants in the single coordinated registry (§3's topology binding), so the reachable-admin set
  is evaluated and preserved by key mutations and root-grant mutations under a **shared
  serialization** — a concurrent `revoke_key(alice)` (observing bob reachable) and
  `revoke(bob's root grant)` (observing alice reachable) cannot both commit across the two
  stores and leave each administrator reachable only through the other's removed half.
  (`DOLMEN_ADMIN_KEY` remains the universal
  recovery, §1.2.)

### 1.6 Credential model (trade-offs on record)

**Humans get expiring signed tokens; machines get revocable named keys.** Deliberately absent:
session store, refresh tokens, email flows, user records — nothing user-shaped to leak, the IdP
owns verification and recovery, and principals stay opaque strings (§1.1).

Accepted losses, on record (amended 2026-09-06):

- No per-device revocation or "where am I logged in" for interactive users: revoking every human
  token at once means rotating the signing secret — rare, and acceptable at the small-team tier
  source B serves.
- No sliding sessions: a token lives out its TTL, then the dance reruns.
- Enterprises are unaffected — the gateway tier (source A) does all of this at the proxy.

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
in §3, `whoami` and the key ops in §1.4–1.5):

| Operation | Verb required | Object checked |
|---|---|---|
| `list_namespaces` | none (any authenticated principal) | Response lists only namespaces the caller holds **any** grant on or under (§3.3). |
| `create_namespace` | `admin` | The **parent**: `*` for depth-1 namespaces, the containing namespace for deeper ones. Under `auth: on` this is the only way a namespace comes to exist (see below). |
| `drop_namespace` | `admin` | The namespace itself. Leaf-only (§5.4). |
| `list_tables` | none (any authenticated principal) | Authorization runs **before** the existence check: unless the caller holds any grant on or under the namespace, the response is `not_found` — indistinguishable from a nonexistent namespace, so listing cannot be used to enumerate names. Holders see the tables they hold any grant on. |
| `describe_table` | any verb (`read`, `create`, `update`, `delete`, `schema`, `admin`) | The table. `row_count` follows the caller's visible set (§4.3) — table-wide for `read`, own rows for the other data verbs on `row_access` tables, 0 for `schema`/`admin` holders; no data visibility beyond the caller's set is implied. |
| `describe_server`, `infer_schema` | none (any authenticated principal) | Untargeted: provider status is no secret; `infer_schema` is pure computation. `describe_server` is **extended, not replaced** (amended 2026-09-06), **under `auth: on` only** — under `auth: off` its response stays byte-identical v0.2.0 (§8.1): the extension reports the auth mode, the enabled identity sources — read-only, no secrets (names like `trusted-proxy`/`oidc`/`api-keys`, never key material or issuer secrets) — and the engine's **capability surface**, including vector-search execution (exact, or declared-ANN with its recall bound, §7) — the established provider-status pattern applied to identity and engine capabilities. |
| `whoami` | none (any authenticated principal) | Untargeted self-description: the caller's principal and groups (§1), whatever the source. The teaching-error philosophy applied to auth — an agent that just got a `403` self-diagnoses in one call. `auth: on`-only (meaningless without identity; see transport parity below). |
| `create_key`, `list_keys`, `revoke_key` | `admin` on `*` | Untargeted (§1.5): a key bears any principal and optional groups, so minting one is administrative at the root — above any one namespace — even though the grants the minted identity can use still have to be granted separately. `auth: on`-only. |
| `create_table` | `schema` | The namespace. |
| `drop_table`, `migrate`, `list_migrations` | `schema`; `drop_table` additionally requires `admin` under `auth: on` — §3.4 makes a table drop delete every grant targeting the table, and changing what others may do is the `admin` verb, not `schema` | The table. Migration history is the audit trail of schema changes — same verb as the changes themselves. |
| `insert` | `create` | The table. |
| `update` | `update` | The table. |
| `delete` | `delete` | The table. |
| `upsert`, `upsert_by_key` | `create` **AND** `update` (both required) | The table. They are update-or-insert: a `create`-only caller is refused up front, not surprised by half the operation. |
| `query` | `read` | The **namespace** — raw SQL may reference any table in it, so the grant must cover the namespace, not one table. See §4.4 for the extra rule on `row_access` tables. |
| `changes_since`, `wait_for`, `subscribe` | `read` on the selected table(s); **or** any data verb (`create`/`update`/`delete`) when the table declares `row_access` — the feed then covers own rows only, mirroring the search rule | The **selected target**: a table-filtered feed checks the named table(s) (direct table grants qualify — inheritance is downward-only); an unfiltered namespace feed checks `read` on the namespace, the same rule as `query`. Per-event scope and credential reevaluation further restrict delivery — foreign rows never wake the caller. Ordinary data ops present in **both** modes (§9.4). |
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
`create_namespace` is the only creation path. The gate is `admin` on the **parent** (`*` for
depth-1, the containing namespace for deeper), never `schema`: `schema` on a namespace creates
tables — content — never sub-namespaces — tenancy. Namespaces are the objects grants hang on
(§3.1); minting one is administrative, and parent-`admin` inherits down (§3.3), so a legitimate
creator always already administers what they create. Under `auth: off` nothing changes — implicit
creation stays, exactly as v0.2.0, permanently.

Transport parity: `grant`/`revoke`/`list_grants` are ordinary ops — same envelope over `/v1/{op}`
and MCP `tools/call`. The full `auth: on`-only surface is those three plus `whoami` and the key
ops of §1.5, and the `/v1/auth/begin` + callback browser endpoints of §1.4 (JSON-envelope ops and
one deliberate non-JSON pair, respectively). With `auth: off` **none of it exists**: not
dispatchable, absent from `tools/list` and `/v1/openapi.json`, keeping the auth-off surface
byte-identical to v0.2.0. The realtime ops of §9 are the deliberate contrast — data ops, not auth
surface, present in both modes (`wait_for` on both `/v1/{op}` and MCP; SSE `subscribe` is
HTTP-surface like `/mcp`).

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
  `409 conflict`-family error with a teaching message. The guard counts **usable** root grants
  only: principal subjects — or group subjects **locally proven by an active key's stored
  groups** (§1.2's decidable exception; §1.5) — mirroring §1.2's usability rule. An
  externally-membered group grant never counts toward
  "another administrator exists" (its membership is external and may be empty or retired), so the
  final principal root-admin grant stays un-revocable even when group root grants are also
  present; otherwise that principal could revoke its own root grant while the guard pointed at an
  unusable group grant, leaving the running server without an administrator and the next restart
  a startup failure. Nor does an **unreachable** principal grant count: "another administrator
  exists" means another usable root grant **reachable under §1.2's rule** — in an API-key-only
  deployment, alice cannot revoke her own root grant under cover of bob's grant while no active
  key bears bob's principal; the revocation would leave no credential able to exercise root
  admin, locking out the running server and failing the next startup's reachability check. This
  makes the bootstrap flow's advice to
  drop `DOLMEN_ADMIN_KEY` after the first grants permanently safe. This guard and §1.5's key
  guard share one serialization: the reachable-admin invariant is evaluated under a single lock
  spanning the grant and key regions of the coordinated registry, so a root-grant revoke and a
  `revoke_key` cannot interleave to strip both halves of the last administration. No guard
  below `*`: an
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
  guarantee. The tombstone records the deleted object's **lifetime key** — `(NsGen, Table,
  DropGen)` for a table, `nsGen` for a namespace; the schema `Version` is deliberately excluded,
  exactly as grant bindings and idempotency records exclude it, because a migration authorized
  by a surviving ancestor grant during the pending window must not make the still-live lifetime
  look "gone" and trigger a spurious finalization. Recovery finalizes whenever THAT lifetime is
  gone — even if a same-named successor already exists (a crash
  after the engine deletion but before cleanup, with a concurrent recreate, would otherwise roll
  the tombstone back on "name exists" and restore the predecessor's grants onto the successor).
  The tombstone also captures the **exact grant rows** it covers as a fixed set at creation:
  exclusion-from-evaluation and physical removal apply to exactly that set and nothing else —
  grants minted after tombstone creation (which, per §3.4's existing-objects rule and the
  incarnation guards, can only target the successor) are never denied or deleted by the
  predecessor's cleanup.
- **Grants target existing objects only** (`*` excepted as the root): no pre-provisioning grants
  against nonexistent namespaces — create, then grant. And grant/revoke mutations themselves
  carry the **target lifetime the request was authorized against** (from the state read),
  verified atomically against the object's **current** lifetime at mutation time — on the update
  path by comparing with the existing row's recorded binding, and on the insert path (no existing
  row) by the same currency check: a mutation that validated against a predecessor, paused, and
  resumed after another replica completed a full drop/recreate cycle (tombstone recorded,
  finalized, name recreated — no tombstone pending, successor grant not yet created) can neither
  merge into or revoke the successor's row on name-based `(subject, object)` identity alone, nor
  **plant** a stale predecessor-bound row that would then `409` every later legitimate grant for
  the same `(subject, object)` indefinitely. Comparing against an existing row's binding alone
  does not cover the no-row path; the insert verifies currency exactly as the update does.
  A lifetime mismatch is `409` — re-read, re-issue.
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
date/time functions accept only **explicit time values and deterministic modifiers**.
`'now'`, `'localtime'`, `'utc'`, and every other wall-clock or host-timezone-dependent form are
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
`fulltext_reindex_rows` redacted. So does **removal of the last full-text index** — `set_fulltext`
disabling the table's only full-text field drops the populated shadow index without recreating
it; latency, I/O, and failures read on hidden corpus size exactly like a rebuild. And **every
`add_field` carrying a backfill `default`** — not
only vectorized ones: apply runs `UPDATE` over every existing row, so the work, latency, and
storage impact read on hidden population, and the caller can re-probe by dropping and re-adding
fields. So does **every vector-clearing path** — `set_vectorize` disabling and dropping the
vectorized field: apply nulls `_embedding` across every row, so runtime, I/O, and failures read
on hidden population. So does **every `drop_field`** — even an ordinary non-FTS, non-vector
column drop rewrites storage across every hidden row, and add-nullable-then-drop cycles make the
probe repeatable. And **`add_field` of a `vectorize: true` field even WITHOUT a backfill
default** — apply still runs the table-wide pending-embedding scan, so latency and I/O read on
hidden population even though every new value is NULL. All other migrations are
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

The seam extracted by the engine-seam stream (SQLite adapter #1) — and, with §0.5, the **tenancy portability guarantee**: everything the
contract says about namespaces, isolation, and row visibility holds on every engine. Everything
above it — envelope, error mapping, validation, authn/authz,
visible-set computation, skills/MCP/OpenAPI rendering — is engine-neutral and shared. SQLite becomes
adapter #1 with **zero contract change** (the conformance suite is the proof).

What stays above the seam: `infer_schema`, `describe_server` (pure/no engine), grant ops and the
grant store (server-level), identity and `RowScope` computation. What the engine owns: everything
namespace/table/row below.

### 6.1 Operation set

Namespace lifecycle, table DDL, row CRUD, filtered reads, migrate ops, search execution, change-log
access (§9) — mirroring today's `*store.Store` methods plus the realtime seam.

### 6.2 Signature sketch

Sketch only — the engine-seam stream owns the final signatures; the operation set and the `RowScope`/paging
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
    // Bootstrap: the AuthBinding set handed to NamespaceState/TableState is
    // WHAT THE AUTHORIZATION LAYER PROVED — one binding per grant that
    // CONTRIBUTED to the operation's required verbs, never caller-supplied
    // (§3.4). For conjunctive requirements (upsert = create AND update;
    // drop_table = schema AND admin), EVERY contributing binding is
    // verified: one stale contributor (e.g. a direct-table admin grant
    // naming the predecessor's DropGen while a namespace schema grant still
    // matches) denies the whole state acquisition — no arbitrarily selected
    // grant can launder the others. The set also includes every binding that
    // DERIVES A SECURITY-SENSITIVE OPTION — the RowScope and the
    // WriteOpts.TableWideRead of the call site — even when its verb is not
    // required to dispatch the operation: a caller holding `update` through
    // a surviving namespace grant and `read` through a direct grant on a
    // predecessor `row_access` table must fail the acquisition on the stale
    // read binding after a drop/recreate, not carry the nil scope it implies
    // onto the successor and update every owner's rows; verifying only the
    // required-verb bindings would launder exactly that. Targeted grants
    // mismatch successors;
    // inherited grants verify their ANCESTOR's (path, nsGen) while the call
    // returns the TARGET's current generation; a Root (*) grant verifies
    // nothing. Empty/zero = no guard (auth off).
    //
    // type AuthBinding struct {
    //     Root        bool       // matched a * grant
    //     Ancestor    string     // ancestor namespace path, when inherited
    //     AncestorGen [16]byte   // that ancestor's nsGen at grant time
    //     TargetGen   [16]byte   // target's nsGen, for namespace-targeted grants
    //     Table       string     // table name, for direct table grants
    //     TableNsGen  [16]byte   // the table's NAMESPACE generation at grant time
    //     TableDropGen int64     // that table's DropGen at grant time
    // }
    // TableState verifies the FULL applicable binding: a direct table grant
    // carries the complete lifetime key (TableNsGen, Table, TableDropGen) —
    // TableDropGen alone is not enough, because a whole-namespace drop and
    // recreate resets table drop generations and a same-named successor can
    // repeat the predecessor's value; the namespace generation is what
    // separates the two.
    NamespaceState(ctx context.Context, ns string, auth []AuthBinding) ([16]byte, error)
    // bindings = the caller's matched grants (as ListTables): verified
    // atomically with the listing, so visibility granted by a direct
    // namespace/table grant bound to a predecessor lifetime cannot surface a
    // recreated successor namespace.
    ListNamespaces(ctx context.Context, prefix string, bindings []AuthBinding) ([]string, error)
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
    // TableState is itself authorization-bound via the same AuthBinding
    // (§3.4 lifetime keys), verified atomically with the read — otherwise a
    // stale grant could fetch the SUCCESSOR's incarnation here and pass it
    // to a later scoped operation, laundering expired authorization through
    // the very call that mints the guard. Zero = no guard (auth off).
    TableState(ctx context.Context, ns, table string, auth []AuthBinding) (*schema.TableSchema, Incarnation, error)
    // bindings = the caller's matched grants (§3.4 lifetime keys): each is
    // verified atomically with the listing, and a direct-table binding's
    // (TableNsGen, Table, TableDropGen) is checked per listed table — a
    // same-named successor recreated within the namespace (nsGen unchanged)
    // must not appear under a grant naming the predecessor's DropGen.
    ListTables(ctx context.Context, ns string, bindings []AuthBinding) ([]string, error)
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
    // Update returns UpdateResult, not a bare count: the count AND the change
    // records minted by the same transaction (below) — the op layer cannot
    // reconstruct what a mutation changed without leaking engine storage above
    // the seam, and a count alone cannot notify (§9.3).
    Update(ctx context.Context, ns, table string, filter string, args []any, set map[string]any, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (UpdateResult, error)
    // DeleteOpts carries the v0.2.0 safety guard (dry_run, limit, confirm) and the
    // engine enforces the threshold inside the delete transaction — an API-layer
    // preflight would race. DeleteResult keeps the contract's matched/deleted pair.
    Delete(ctx context.Context, ns, table string, filter string, args []any, opts DeleteOpts, scope *RowScope, scopeIncarnation Incarnation) (DeleteResult, error)

    // Change log (§9). The durable per-namespace log is ENGINE-OWNED — records
    // and cursors are minted inside the write transaction (§9.3) — so both
    // access paths cross the seam:
    // 1. Every write result (InsertResult, UpdateResult, DeleteResult) carries
    //    the []ChangeRecord minted by its own transaction: {Cursor, Table,
    //    RowID, Kind (insert/update/delete), Owner, Lifetime (the table's
    //    lifetime key, §9.3)}. Owner is INTERNAL
    //    authorization metadata stamped from the row (§9.3) — delete events
    //    cannot be scope-filtered from a row that no longer exists, and
    //    historical replay cannot consult current row state. It never appears
    //    in public payloads unless the row itself would be visible.
    // 2. ChangesSince is the scoped replay read: table != "" applies the
    //    table-authorized feed rule (per-record scope via the Owner label);
    //    table == "" is the namespace feed, guarded by nsGen exactly like
    //    Query. Returns records in cursor order plus the next cursor.
    ChangesSince(ctx context.Context, ns, table string, from Cursor, nsGen [16]byte, scope *RowScope, scopeIncarnation Incarnation, page Page) ([]ChangeRecord, Cursor, error)
    // Listen is the engine-declared notification capability (B4-style), and
    // it is CURSOR-AWARE so the seam itself provides §9.3's atomic
    // register-and-replay: the engine registers the listener and fixes the
    // replay boundary at the registration point as ONE coordinated
    // operation. Replay is PAGED, not one slice — an old-but-retained
    // cursor must not let a client force an unbounded allocation — ChangeReplay
    // pages with the same conventions as ChangesSince, preserving the
    // atomic boundary throughout. Order is cursor order, period:
    // `notify` is NOT invoked until the caller has drained the replay to
    // the registration point — records committing in the interim buffer
    // (a bounded queue) and are delivered after, so the concatenation
    // replay-then-live is exactly cursor order and a client persisting only
    // its last-delivered cursor can never skip older records. Per-event
    // authorization — the caller's RowScope and its incarnation, passed in —
    // runs BEFORE queue admission: foreign records never enter the handoff
    // queue at all, so they cannot fill it, displace, or starve a scoped
    // subscriber; §9.3's foreign-rows-never-wake rule holds under backpressure
    // too, and the overflow-reconnect below can only ever be triggered by the
    // subscriber's own visible traffic. If the client
    // drains the replay slower than writes arrive and the interim buffer
    // bounds, the engine closes the stream with the teaching reconnect
    // recipe (resume from the persisted cursor — the log is durable; the
    // buffer never is the durability mechanism). An event committed before
    // registration exists only in the replay, one committed after only in
    // the stream — exactly once across the two halves, boundary dedup is
    // the engine's, inside the atomic operation. An op-layer
    // replay-then-listen split would retain the
    // commit-before-registration gap; a listen-then-replay split would need
    // an unspecified buffering protocol — neither is conforming. Engines
    // without the capability degrade — wait_for
    // still meets its bounded-time contract via internal scanning, subscribe
    // may be declared unavailable — surfaced like every other engine
    // capability; notification is never the durability mechanism (§9.3).
    Listen(ctx context.Context, ns, table string, from Cursor, scope *RowScope, scopeIncarnation Incarnation, notify func(ChangeRecord)) (*ChangeReplay, cancel func(), error)

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
identical response. **One exception, `search_vector` only (amended 2026-09-06):** an engine MAY
run vector search over an ANN index (e.g. pgvector HNSW) as a first-class **accelerator**, and
when it does the ranking is *approximate* — it may differ from exact brute-force within a
documented recall bound, and the capability MUST be **declared** (B4-style, like §9's
notification capability), never silent — and the declaration is **public on two channels**, both
`auth: on`-only so the auth-off response shapes stay byte-identical v0.2.0 (§8.1; adapter #1
under `auth: off` is exact brute-force and reports nothing new): the
engine's capability surface (`describe_server`, §2, advertises vector execution as exact or
ANN-with-recall-bound) and **per-result execution metadata** on every `search_vector` response
(which path served it — an engine that switches between ANN and exact fallback as an index
builds or query conditions change says so on each response). The contract then pins the result
**shape**, the
**visible set**, and **determinism** — the ranked sequence is a property of
**(query, corpus, index state) alone**: one total order with a deterministic `id` tiebreak,
independent of the requested `limit`/`offset` — every pagination request reads a window of THAT
sequence, and the engine may not let the requested K influence which candidates the index
retrieves (a deterministic-but-K-dependent candidate set would still duplicate or omit rows
across `offset` pages). An engine that cannot guarantee a K-independent stable sequence must
not serve offset-paginated ANN results — exact fallback instead — not
bit-identical ranking across engines or index configs. What stays exact regardless of index:
the visible set (RowScope filtering), `truncated`, `skipped_vectors`, pagination — approximation
affects ordering among the top-K only, never which rows are eligible. Under a `RowScope`, the
ANN **candidate generation itself runs over the already-filtered visible corpus** —
scope-partitioned indexes or equivalent prefiltering; post-filtering an unscoped candidate set
is NOT conforming, because foreign vectors consuming the candidate budget would let another
tenant's writes reorder or displace the caller's results (the §7 scope rule) and turn the index
into a cross-tenant existence oracle; an engine that cannot prefilter safely serves that query
from the exact path. The **brute-force exact
path remains the conformance reference**: the canonical-cosine arithmetic and
`q(s)=floor(s/fl64(1e-9))` quantization below stay the *definition of the exact path* — they
stop being the only permitted execution strategy. §8.1's byte-identical auth-off corpus is
untouched: SQLite under `auth: off` stays exact brute-force. Full-text search is out of scope
for this exception — every full-text path stays exact per the rule above. Shadow structures
(FTS5 tables, ANN stores)
never appear in `list_tables` or any other surface.

What the contract pins (conformance-enforced on every engine):

| Property | Contract |
|---|---|
| Result shape | Rows as stored plus `id`/`created_at` (and `owner` when present), typed reads per field type; `_score` on every vector result (cosine; `-1..1` is the **auth-on clamped guarantee** — under `auth: off` the raw value is preserved and may marginally exceed the range, per Ordering); no rank value exposed for fulltext. |
| Ordering | **Two tiers.** Under `auth: off`, adapter #1's v0.2.0 behavior is preserved bit-for-bit (§8.1): raw cosine — unclamped, so a self-comparison may report `1.0000000000000002` — exact comparisons, and full-text ordered exactly as v0.2.0 executes it, `ORDER BY rank, rowid` (the explicit `rowid` tiebreak, not "native tie order"); nothing in this row redefines that mode, and engine-2 passes the golden auth-off corpus by reproducing adapter #1's exact arithmetic and ordering (which the canonical accumulation below already matches). With `auth: on`, and for cross-engine conformance generally: `search_fulltext` orders by **`q(rank)` ASCENDING** — FTS5 rank is lower/more-negative for more relevant rows, so ascending buckets ARE relevance-descending — tiebreak `id` ascending; `search_vector` orders by `q(_score)` descending, tiebreak `id` ascending — via **canonical arithmetic, then transitive quantization** (the exact path's rule; a declared-ANN engine's approximate ordering may differ within its documented recall bound per the accelerator exception above, while shape, visible set, and pagination stay exact). Canonical cosine, fully specified: BOTH operands are normalized to float32 first — the query
vector (raw caller-supplied or provider-embedded) is rounded to nearest float32 exactly as
adapter #1 does today, matching the stored float32 vectors — then computed in binary64; dot products and squared norms accumulated component-wise in dimension order; every multiply and add individually rounded (IEEE-754 round-to-nearest-even) — fused multiply-add/contraction and reassociation are FORBIDDEN; norms via the correctly-rounded square root; if either vector has zero norm the score is exactly `0` (never NaN, never skipped); the final quotient clamped to `[-1, 1]`. Engines may use faster internal paths only if the canonical value (and hence its bucket) is identical; the conformance corpus verifies. Quantize the canonical value with prescribed arithmetic: `q(s) = floor(s / fl64(1e-9))` — the divisor is the binary64 value nearest `1e-9`, the division is one correctly-rounded binary64 operation, then `floor` — computed in binary64 by EVERY engine regardless of internal storage (a decimal-backed engine evaluating the division exactly would put `q(0.5)` in bucket 500000000 where the prescribed binary64 division yields 499999999; the binary64 result is the contract). Order by the integer `q(_score)` (ties break by `id` ascending); `min_score` is constrained to the cosine range `[-1, 1]` (`invalid_request` outside), and the threshold compares `q(s) ≥ q(min_score)`. Quantizing per-engine approximations would NOT be consistent, and pairwise epsilon is non-transitive; canonical-then-bucket avoids both. Identical corpus + query ⇒ identical order on every engine. Under a scope, ranking operates over the **visible corpus only**: relevance statistics must not include rows outside the caller's visible set — foreign matching rows can never reorder or displace visible results (§4.3). Predicate conjunction alone is not sufficient (a shared index's corpus statistics span owners); engines choose the isolation — per-scope index partitioning, or filter-then-rescore. |
| Match language | **Two tiers** (amended 2026-09-05). **Core subset — contract on EVERY engine**: terms, implicit AND, `OR`, `NOT`, quoted phrases, `term*` prefix — with the **grammar pinned to SQLite FTS5's documented parsing rules and complete precedence ladder** (`NOT` is a BINARY exclusion operator — `a NOT b` matches rows matching `a` but not `b`, and bare leading `NOT b` is a syntax error; precedence, tightest to loosest: **implicit AND (juxtaposition), then `NOT`, then explicit `AND`, then `OR`** — so `a NOT b AND c` groups `(a NOT b) AND c` while `a NOT b c` groups `a NOT (b AND c)`; binary operators left-associative within their level; parentheses group; phrase interiors are token sequences with no operators; prefix `*` applies to the immediately preceding term) — identical semantics across engines, conformance-enforced, with the documented tokenizer/stemmer behavior (porter over unicode61: case/diacritic folding, English stemming, opaque CJK runs). **Extended grammar — engine-documented, not guaranteed portable**: `field:term`, `{a b}:term`, `NEAR(...)`; SQLite (adapter #1) supports all of it natively, other engines may implement any of it, and engines document which extended constructs they accept — unsupported extended syntax fails with the same teaching-error quality as everything else. *Rationale: requiring a scan-and-score engine to reimplement the full FTS5 grammar is lift without a demander; the core subset covers observed agent usage.* |
| Ranking quality | The normative full-text ranking is SQLite FTS5's built-in BM25 (`rank`) with default parameters — k1=1.2, b=0.75, all column weights 1.0 — computed over the visible corpus. "BM25-family" is not a license for a variant: differing idf definitions, length normalization, parameters, or field weights reorder the same corpus while still feeling like BM25. Two tiers, mirroring Ordering: under `auth: off`, adapter #1's explicit `ORDER BY rank, rowid` is preserved bit-for-bit per §8.1. With `auth: on`, the normative rank values are adapter #1's emitted ranks, bit-for-bit — SQLite FTS5 as the reference oracle, other engines reproduce its output exactly (corpus-verified on fixtures) — and near-equal scores compare with the same canonical quantization as vectors, `q(rank) = floor(rank / fl64(1e-9))` with `id` tiebreak, so precision, evaluation order, and log rounding cannot reorder documents or shift offset pages between engines. |
| Truncated / pagination | `limit` default 10 max 200, `offset`, `truncated` exactly as v0.2.0 — always computed over the caller's visible set (§4.3). |
| Vector skips | Corrupt/dimension-mismatched/non-finite stored vectors are skipped and reported as `skipped_vectors`, never silently dropped. |

The SQL dialect of `filter`/`args` on searches and of `query` is SQLite SQL on adapter #1; engine-2's
dialect stance is engine-2's issue, and must keep the conformance corpus green.

## 8. Dual-mode conformance plan

The conformance suite (`internal/conformance`) runs in every mode in CI (`make test`); breaking
any mode fails CI.

### 8.1 What "byte-for-byte v0.2.0" means, precisely

The existing conformance package — every request and pinned response it contains — runs against an
`auth: off` server and passes **unmodified**: every request v0.2.0 accepts behaves identically, and
nothing v0.2.0 accepted may change meaning. Additive extensions are permitted — optional request
keys (`prefix`), newly valid inputs (deep namespace paths), new ops — and the schema documents may
grow accordingly (`list_namespaces` gaining `prefix`, the namespace pattern widening to allow `/`),
provided every schema change is strictly additive: each request that validated against the v0.2.0
schemas still validates and produces the same response. What never appears under `auth: off`:
`grant`/`revoke`/`list_grants` in dispatch, `tools/list`, or `/v1/openapi.json`; the identity
surface of §1.4–1.5 (`whoami`, `create_key`/`list_keys`/`revoke_key`, and the `/v1/auth/begin` +
callback endpoints) likewise; and the `row_access` annotation in any schema.

### 8.2 Modes in the harness

The matrix has three modes (grown from two, 2026-09-06) — the harness is already
mode-parameterized, so this adds fixtures, not machinery:

- `auth: off` — today's harness, unchanged: full suite, golden contract.
- `gateway` — `DOLMEN_AUTH=on`, `DOLMEN_TRUSTED_PROXIES=127.0.0.1/8`, and an admin key; tests
  assert identity via `X-Dolmen-Principal`/`X-Dolmen-Groups` (or the bearer key) the way a
  gateway would.
- `native+keys` — `DOLMEN_AUTH=on` with the OIDC source enabled against a **local issuer stub**:
  fixtures run the real dance (`/v1/auth/begin` → stub → callback → bearer token) and exercise key
  issuance (`create_key` → use → `list_keys` → `revoke_key` → 401). **Arrives with the native-OIDC
  stream** — source B is designed-not-built (§1.4/D21), so this mode is contingent on that stream
  landing; until then the matrix runs `off` + `gateway`, and the key-issuance fixtures may ride
  earlier (API keys have no such dependency).

### 8.3 What auth:on adds to the suite

1. **Deny-by-default sweep** — for every op (all 29 with §1.4–1.5 and §9; `/v1/auth/begin` and
   the callback are excluded — unauthenticated by construction, §1.2): no identity (401
   `unauthorized`) and untrusted-peer identity (401). Authenticated-but-ungranted (403
   `forbidden`) applies to the **grant-protected** ops only. The grant-free ops of §2 succeed for
   an ungranted authenticated caller (`describe_server`, `infer_schema`, `list_namespaces`,
   `whoami` — `list_namespaces` returning an empty list) — except `list_tables`, which per §2
   returns `not_found` for a caller holding no grant on
   or under the namespace; the sweep asserts exactly that. Envelope shapes pinned like every other
   error.
2. **The acceptance scenario, end-to-end** — one test scripting the umbrella scenario:
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
6. **Source-blindness** (added 2026-09-06) — the grant-matrix subset of item 3 runs identically
   under a header identity, an OIDC token, and an API key for the same `(principal, groups)`:
   identical verb decisions, identical RowScope, identical responses. Downstream must not be able
   to tell sources apart — that is §1's seam claim made conformance-enforced. The native-mode
   fixtures (§8.2) supply the token and key identities.
7. **Realtime cases** (added 2026-09-06, §9 — in **all** modes, auth-off included): event-on-write
   (a commit produces the change record, in §0.6 order); cursor replay (a restarted client
   replays from its stored cursor, gap-free); scope filtering (a foreign row's commit never
   delivers an event to a scoped caller, on every one of `changes_since`/`wait_for`/`subscribe`);
   per-event scope evaluation (a grant revoked mid-subscription drops the stream — auth:on);
   `wait_for` timeout returns **empty, not an error**; reconnect catch-up
   (`changes_since` + re-subscribe equals the missed events); a cursor beyond retention is the
   documented teaching error.

### 8.4 CI wiring

**Every matrix mode that has landed runs in CI** — `auth: off` + `gateway` from day one,
`native+keys` when the OIDC stream lands (§8.2): skipping an available mode fails CI, so the
native fixtures (source-blindness, the OIDC dance, key issuance) can never silently drop out.
Each is an ordinary `go test ./...` run inside `make test` — mode selection happens inside the
harness per test group, no CI matrix, no new make targets.

## 9. Realtime change notifications

(Amended 2026-09-06 — auth, authz, and subscription are designed together: the standing-read,
scope, and emission semantics interlock with §4.3, §0.6, and the engine-capability rules.)

### 9.1 The pattern

Invert dolmen from queryable memory into the **wake-up channel for 24/7 agents**: appenders
(email connectors, webhook catchers, cron, other agents) write events; sleeping agents get woken
by the rows they care about instead of polling. This kills the poll-every-N-minutes token burn —
the sleeping agent holds nothing, burns nothing, and is told.

### 9.2 Four layers, each independently useful (build order)

1. **Change log (foundation):** a durable **per-namespace monotonic sequence**, assigned at
   commit. Op `changes_since(namespace, cursor, [table filter])` → ordered change records. This
   alone makes even polling cheap and incremental.
2. **`wait_for` (long-poll op):** blocks up to a bounded time (default ≤60 s, configurable) until
   a matching change commits; returns immediately when one lands; **empty result — never an
   error — on timeout**. Fully agent-usable over plain MCP/HTTP today: one tool call per wait, no
   special client.
3. **`subscribe` (SSE stream):** server-sent events on the HTTP surface — change type
   (insert/update/delete), row ids, optional row payload, filtered by the caller's scope. The
   payload is a live-notification convenience and is **not replayable** — the durable record
   carries the change's identity and authorization metadata, never a row snapshot (§9.3).
   Reconnect passes the cursor: listener registration and replay are **atomic** (§9.3). For
   agent **hosts** holding connections: an LLM turn cannot hold a connection; a framework can.
   Transport note: like `/mcp`, this is an HTTP-surface capability — the MCP tool surface gets
   `wait_for` (its request/response shape); SSE `subscribe` is host-side.
4. **Webhooks (designed, not built):** outbound delivery for server-side agents behind NATs —
   retry machinery and receiver authentication; recorded as a deferred layer with this note, the
   same status as portability and audit (§0.6): no demander yet for dolmen-side outbound
   delivery.

### 9.3 The three commitments (cheap now, retrofit-expensive — pinned precisely)

- **A subscription is a standing read.** `changes_since`/`wait_for`/`subscribe` authorize against
  the **selected target** (§2's verb table carries these rows): a table-filtered feed requires
  `read` on the named table(s) — **or, when the table declares `row_access`, any data verb**
  (`create`/`update`/`delete`), the feed then covering own rows only, mirroring the search rule
  (§2) so a `create`-only telemetry writer can wait on their own appends. Direct table grants
  qualify (inheritance runs downward only, so
  the namespace-level check would wrongly deny them); an unfiltered namespace feed requires
  `read` on the namespace — the same rule
  as `query` (§4.4). Every event is filtered through the caller's visible set — **foreign rows
  never trigger a wake-up and never appear in a change record**. Scope is evaluated **per event,
  not just at subscribe time**: a grant revoked mid-subscription stops the stream; a `row_access`
  or schema change re-resolves the scope against current grants (composing with §4.3's
  incarnation/version-guard pattern — the guard binds even to the standing stream, not only to
  point reads). And the **credential is revalidated per event too**, not just the grants: a
  revoked API key drops the stream at the next event (best-effort immediately), and an OIDC
  token's expiry bounds the connection lifetime — the stream closes no later than token expiry
  with a teaching close, and reconnect with a fresh token resumes from the durable cursor;
  `wait_for`'s bounded window makes the same revaluation implicit at every return. Source A has
  no credential state to revalidate — its identity is headers asserted once on the request that
  opens the stream, and dolmen holds no gateway-session or group-membership state (§0) — so
  source-A streams are **bounded by `-max-subscription-age`/`DOLMEN_MAX_SUBSCRIPTION_AGE`**
  (§1.2: default `30m`, valid `0` or `1s`–`24h`, startup-rejected outside; `0` disables the bound
  and with it the backstop): at the bound
  the server teaching-closes and the client reconnects, which re-asserts headers and thus
  refreshes the identity cheaply (cursor resume). The deployment obligation is documented
  alongside §1.2's reachability assumption: the gateway **must terminate the upstream connection
  when the asserted identity's session ends or its groups change** — dolmen cannot observe it,
  and the duration bound is the backstop, not the enforcement.
- **Change records are written atomically with the write they describe; notification happens
  after commit.** The durable log record and its cursor are assigned **inside the same
  transaction as the data write** (adapter #1: the log lives in the same SQLite database file, so
  this is natural; an engine where co-transaction is awkward may use an equivalent
  transactional-outbox design). There is no crash window in which an acknowledged write is
  missing from the log — there is no "between" — and concurrent handlers cannot append records
  in an order different from their commits, because the same serialization point that orders
  commits (§0.6) assigns the sequence. Every write already funnels through dolmen, so there is no
  CDC machinery on SQLite — engine-neutral by construction. Event order is the §0.6
  serial-observability order; the cursor reflects commit order. Every record captures the row's
  **owner as internal authorization metadata**, stamped in the write transaction (absent on plain
  tables with no `owner` column) — a delete event cannot be scope-filtered from its row after the
  fact (the row is gone), and historical replay cannot consult current row state once a later
  delete removes it; without the label an implementation must either leak foreign delete events
  or suppress legitimate own-row ones. The label never appears in public payloads unless the row
  itself would be visible. What the op layer does after
  commit is only **notification** — waking `wait_for` callers and pushing SSE frames — and
  notification loss is harmless: the durable log is complete, and `changes_since` recovers
  every **change** a missed wake would have signaled — identity, cursor, kind, owner — but not
  the transient payload bytes of a live SSE frame: the record stores no row snapshot (a later
  update or delete would falsify one), so a replaying client re-reads current row content by id
  through its standing read. Cross-pod fan-out on
  shared engines is an **engine-declared capability** (B4-style topology rule): single-process
  works day one; engines without a notification bus may declare `subscribe`/`wait_for`
  unavailable or degraded, surfaced like every other engine capability — with one division that
  is NOT the engine's choice: **`wait_for` is never unavailable** — it degrades to internal
  scanning and always retains its bounded-time contract (§6.2, §9.2); only `subscribe`, which
  requires a held connection, may be declared unavailable.
- **Cursors are durable.** A restarted agent replays from its cursor; reconnect =
  `changes_since` catch-up + re-subscribe — and the server side makes that sequence
  race-free: **`subscribe` accepts the cursor and atomically registers the listener and replays
  from it as one operation**. A write committing after a standalone catch-up read but before
  listener registration would otherwise have no recipient and could sleep the new stream
  indefinitely despite sitting in the durable log; the atomic form closes that window, and the
  manual two-step is a client convenience, not the recovery guarantee. Cursor semantics:
  **per-namespace, monotonic,
  gap-free**; a cursor pointing beyond retention is an explicit teaching error naming the
  catch-up path. Pruning/retention of old change records is a configuration concern whose knob
  is **time-based expiry only, never a record-count or byte cap**: a volume-based limit would
  let foreign commits evict a scoped reader's cursor while that reader has received nothing
  visible, and the resulting beyond-retention error would reveal that hidden namespace traffic
  occurred — precisely the observation the opacity rules below forbid; age-based expiry is
  independent of who wrote what. The cursor is **bound to the namespace lifetime that minted it** — it encodes
  the `nsGen` alongside the sequence position, opaquely: adapter #1's change log dies with the
  namespace database (§5.4's clean slate), so a recreated namespace's sequence restarts, and a
  predecessor cursor replayed against the successor would otherwise silently skip its first N
  records (cursor 5 against a successor that has already emitted ten) — instead it is a lifetime
  mismatch, the same explicit teaching-error family as beyond-retention, naming the catch-up path
  (resume from the current head). And the cursor is **opaque and non-order-revealing to the
  client**: a scoped or table-filtered feed delivers only the caller's visible records, and
  handing back raw namespace-wide sequence positions would expose that a filtered-out record
  existed between two visible ones (cursors 10 and 12 reveal record 11) — client-facing cursors
  are per-feed resume tokens the server maps to its internal position on resume, so consecutive
  visible records yield consecutive tokens indistinguishable from adjacent ones, and no foreign
  commit is observable through cursor arithmetic. The token mapping is **persistent and
  deployment-wide, never per-process**: tokens are self-contained and **encrypted** —
  authenticated encryption (or an equivalent construction that cryptographically hides the
  internal position — a plaintext payload plus a MAC authenticates but does not conceal, and the
  sequence gaps must stay unobservable) — under a
  persistent deployment-wide key (the §1.4 signing-key coordination pattern), or the mapping
  lives in coordinated storage like the registry itself — a token issued by one replica must
  resume on another, and a restart must not invalidate clients' cursors.
  And every record carries its **table's lifetime key** (`NsGen`, `Table`,
  `DropGen` — Version excluded as everywhere): a table-filtered feed delivers only records of the
  table's **current** lifetime, so a caller granted on a recreated same-named successor can never
  replay a predecessor's records from an old cursor — `scopeIncarnation` proves the current table
  is authorized, but only the per-record lifetime label can tell which lifetime produced each
  historical record. (Deleting a dropped table's records instead would punch a hole in the
  per-namespace sequence — retention would become table-scoped, and every table drop would force
  a "beyond retention" error onto mid-replay clients, contradicting the gap-free cursor guarantee;
  labeling with delivery-time lifetime filtering is the only shape consistent with it.)
  Namespace-wide feeds are
  guarded by `nsGen` like `query` and deliver the namespace's own recorded history — their
  readers held namespace read throughout.

### 9.4 Modes

Realtime ops exist in **both modes** — they are data ops, not auth surface. Under `auth: off`
they are additive per §8.1's additive-extension rule (the local agent waiting on its own data is
a first-class scenario); unrestricted, ordinary rules. Under `auth: on`, the standing-read
semantics of §9.3 apply.

### 9.5 Boundary (decided in review)

Message **decoding** (MIME, webhook bodies, protobuf → structured rows) stays OUT of dolmen: the
agent itself decodes for LLM pipelines; fiso (or any mediator) is the codec/transform layer for
non-LLM ones. Dolmen contributes typed fields, coercion, `json` fields, `infer_schema` — not an
ETL layer (an ETL layer in dolmen would be fiso-shaped, not dolmen-shaped).

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
| D15 | Search semantics are contract; index vs scan is engine choice; index never changes semantics (search_vector ANN exception per D26); match language is tiered — core subset (terms, implicit AND, OR, NOT, phrases, prefix) contract on every engine, extended grammar (`field:term`, `{a b}:term`, `NEAR`) engine-documented | §7 |
| D16 | Dual-mode conformance: precise byte-for-byte rule, deny sweep, acceptance scenario, invariant tests, no CI matrix | §8 |
| D17 | Architecture invariants: namespace is a logical tenant coordinate (permissions bind to names, never files); two-level tenancy — projects structural via namespaces (engine-mapped file/schema), users logical via the owner predicate (delegatable to native RLS); user-per-namespace is an anti-pattern; raw-SQL confinement is an engine obligation; Postgres the reference shared engine (adapter #2, D25) | §0.5 |
| D18 | Grant lifecycle: no owner concept (inheritance covers creators); last-admin-on-`*` revoke guard (409); drop cascades grant deletion (clean slate on recreation, count reported in drop confirm); grants target existing objects only | §3.4 |
| D19 | Consistency contract: op atomicity; per-namespace serial observability + read-your-writes; 409-or-nothing collision surfacing; engine-declared deployment topology (sqlite = 1 process/data-dir; shared engines = N pods) | §0.6 |
| D20 | Data portability SKIPPED and audit surface DEFERRED, deliberately — no features without real-world demanders (#32 open, unrescoped) | §0.6 |
| D21 | Identity is additive, pluggable **sources** behind one seam; a request is authenticated when any enabled source yields a principal; everything downstream is source-blind. Trusted-proxy headers = v1; native OIDC designed-not-built (PocketBase-shaped provider abstraction, Ed25519 signed tokens, PKCE+state, principal = `sub` never email, `/v1/auth/begin` + callback as the one non-JSON browser surface); API keys `dlm_…` hashed in the registry; the authn stream builds the seam | §1, §1.4–1.5 |
| D22 | Credential model on record: humans get expiring signed tokens, machines get hashed revocable named keys carrying principal + optional groups; no sessions/refresh/email/user records (deliberate — nothing user-shaped to leak, IdP owns recovery); accepted losses on record — no per-device revocation (revoke = rotate the signing secret), no sliding sessions; enterprises use the gateway tier | §1.4–1.6 |
| D23 | Bootstrap written up explicitly: the deadlock rationale; the admin key is a kingmaker, not a king (implicit grant on the credential, identity vanishes with the env, grants persist; `dolmen-admin` reserved across all sources; removal safe via the root-admin startup check + last-admin guard). Namespace-creation gates: `auth: off` implicit forever; `auth: on` `not_found`-after-authz; `create_namespace` requires `admin` on the parent; `schema` creates content, never tenancy | §1.3, §2 |
| D24 | Realtime change notifications join the spec ("one bag" — auth, authz, and subscription designed together): four layers in build order (durable per-namespace change log + `changes_since` → `wait_for` long-poll ≤60s → SSE `subscribe` for agent hosts → webhooks designed-not-built); a subscription is a **standing read** (`read` verb — or any data verb under `row_access`, own rows only — visible set, **per-event** scope AND credential evaluation, mid-subscription revocation drops the stream, source-A streams duration-bounded with the documented gateway termination obligation); change records and cursors are **minted inside the write transaction** (per-record table-lifetime key and internal owner label; no CDC; notification after commit only, never the durability mechanism; order = §0.6 serial observability; cross-pod fan-out = engine-declared capability); durable gap-free per-namespace cursors with a retention knob (beyond retention = teaching error); realtime ops exist in both modes (data ops, not auth surface); decoding stays OUT (agent decodes; fiso is the codec layer for non-LLM pipelines) | §9 |
| D25 | Postgres is the reference shared engine (adapter #2 — the org/multi-host tier; schema-per-namespace, native RLS for RowScope, role-per-schema confinement, ACID are its native vocabulary; only an operational engine can honor the operational contract). The lakehouse is **read-side, not an engine**: Iceberg/Delta/DuckDB-as-engine reclassified out of the adapter list — analytical stores cannot honor the write contract; dolmen's data may be published to Parquet/Iceberg/Delta via background export/CDC, designed-not-built and demander-gated (D20 status, like webhooks §9) — a bolt-on read path, NOT a `store.Engine` implementation, and DISTINCT from the user-facing `export`/`import` portability ops D20 skips | §0.5 |
| D26 | `search_vector` ANN accelerator exception (full-text stays exact): an engine MAY serve vector search from an ANN index (e.g. pgvector HNSW) with **declared** capability (B4-style, never silent) and a documented recall bound; the contract pins result shape, visible set, and per-path determinism — not bit-identical ranking across engines or index configs; RowScope, `truncated`, `skipped_vectors`, and pagination stay exact regardless of index (approximation affects top-K ordering only, never eligibility); the brute-force exact path (canonical cosine + `q(s)` quantization) remains the conformance reference; §8.1's auth-off corpus untouched | §7 |

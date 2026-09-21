# Changelog

## Unreleased

### Added

- **Idempotency keys are namespaced by owner.** A key is unique per table *and* writer principal,
  so two principals using the same string are using two different keys — neither conflicts, neither
  reveals the other, and the first to use a key cannot squat it. A retry consults only its own
  domain, so it finds its own record through grant changes, gained table-wide `read`, and
  `row_access` disablement alike; the payload comparison that rejects a changed body therefore never
  runs against someone else's record. Records written before `-auth on` are preserved and replay to
  callers holding table-wide `read`, whose rows those ids already are; turning auth off again
  consults only that pre-auth domain, so `auth: off` behaves exactly as it did in v0.2.0. This lifts
  the refusal of `idempotency_key` for a caller restricted to their own rows.

  **This one is a one-way door.** Existing databases gain the new column on first open, and the
  namespace's catalog minimum-reader stamp rises with it, so an older dolmen refuses to open the data
  directory afterwards rather than opening it and reading the table as though keys were still global
  — which is precisely the disclosure being closed, since that reader would answer one principal's
  retry with another's ids. The records also move to a new table rather than growing a column in
  place, so an older dolmen that already had the directory open when the upgrade ran fails loudly on
  its next idempotent insert instead of quietly reading the new layout with its ownerless lookup: the
  catalog stamp can only refuse the *next* open, not a process already running. Back up a data
  directory before the first open if you may need to downgrade.

- **Filters are a row-local language when auth is on.** `update`, `delete`, `upsert`,
  `search_fulltext` and `search_vector` take a SQL `WHERE` fragment; with `-auth on` that fragment
  is now parsed and checked against a fixed allowlist before it reaches the engine — the target
  table's own columns, literals, `?` arguments, and an enumerated set of operators and functions.
  Subqueries, references to another table, aggregates, window functions and anything else are
  `invalid_request`. Two reasons: a grant on one table must not reach the table next door through
  a subquery, and under a row scope a filter that can read or raise on an invisible row is an
  oracle over it. Date and time functions take explicit moments only — `'now'`, `'localtime'` and
  `'utc'` are refused whether written as literals or bound as arguments; compute the moment you
  mean and bind it. Under `auth: off` the filter language is exactly what it was in v0.2.0.

- **`-auth on` / `DOLMEN_AUTH=on`** turns on deny-by-default authentication, with the bootstrap
  admin key (`DOLMEN_ADMIN_KEY`) as its one identity source. Every `/v1/{op}`, `/mcp`, and
  `/v1/subscribe` request without an accepted credential answers `401` with the new `unauthorized`
  error code; rejections are uniform, so the message never says which part failed. `/healthz`,
  `/version`, `/skills*`, and `/v1/openapi.json` stay unauthenticated in both modes. This is a
  shared credential for one administrative principal, not a multi-user system — per-user
  identities, API keys, and permissions are the following slices. `dolmen mcp` refuses to start
  with auth on: a stdio pipe carries no per-request credential.
- **Identity asserted by a gateway.** `-trusted-proxies` / `DOLMEN_TRUSTED_PROXIES` lets peers
  inside the given CIDRs assert `X-Dolmen-Principal` and `X-Dolmen-Groups`. Trust is decided from
  the immediate TCP peer, never from `X-Forwarded-For`; headers from other peers are ignored
  entirely. `-max-groups` / `DOLMEN_MAX_GROUPS` (default 128, range 1–1024) caps the group list,
  and an over-limit list fails the identity rather than dropping a group that might carry a grant.
  An asserted identity authenticates and is then refused `403 forbidden` until the grant ops land —
  deny-by-default, which is what makes the source safe to enable before permissions exist.
- **Grants.** `grant`, `revoke`, and `list_grants` give a principal or group verbs — `create`,
  `read`, `update`, `delete`, `schema`, `admin` — on a namespace, a table, or `*`. Grants inherit
  downward and combine by union, with no deny grants and no precedence. The verb gate covers every
  operation, `/mcp`, and `/v1/subscribe`. `whoami` reports the caller's identity so an agent can
  self-diagnose a `403`. All four ops exist only when auth is on, and are absent from dispatch,
  `tools/list`, and `openapi.json` when it is off.
  Authorization precedes existence: an ungranted caller gets `403` whether or not the object
  exists, `list_tables` answers `404` for a namespace they hold nothing under, and
  `list_namespaces` returns only what they can reach. Implicit namespace creation is disabled when
  auth is on, since it would bypass the parent's `admin` gate. Dropping a namespace or table
  removes the grants targeting it, and revoking the last root administrator is refused while no
  replacement exists.
- **Per-row ownership.** A table declared with `row_access: "own"` carries an implicit `owner`
  column the server stamps on every insert; callers never supply it. A caller holding `read` sees
  the whole table, a caller holding only a data verb (`create`/`update`/`delete`) sees the rows
  they wrote, and a `schema`/`admin`-only holder sees none — `describe_table` reports 0 rather than
  the real count. The annotation exists only when auth is on, and never appears in a schema when it
  is off. Enabling it later through `migrate set_row_access` is refused on a table that already has
  rows, because no operation can write another principal's rows as that principal; turning it off
  keeps the column and its values and requires `admin` as well as `schema` and `read`. Scoped
  `update`, `delete`, `upsert`, `upsert_by_key`, filtered searches, and inserts carrying an
  `idempotency_key` are refused for now: caller-supplied filters, key matches, and a per-table
  idempotency record each need work before a scoped caller can use them without probing rows they
  cannot see.
- **API keys.** `create_key` / `list_keys` / `revoke_key` mint credentials for machines that cannot
  do an interactive sign-in. A key authenticates as a principal and carries optional groups, but
  grants nothing by itself. The credential is shown once and stored hashed; keys are revoked by a
  server-generated id, so two sharing a name stay individually revocable; `list_keys` never returns
  credentials; and a revoked key is refused with the same `401` as an unknown one. Downstream of
  the seam a key identity is indistinguishable from a gateway-asserted one — same verbs, same
  grants, same row scope.
- **Native sign-in through an identity provider.** With `DOLMEN_AUTH_OIDC_ISSUER` (or
  `DOLMEN_AUTH_OIDC_PRESET=github`) set, `/v1/auth/begin` runs the authorization-code flow with
  PKCE and single-use state, and hands back an Ed25519-signed, stateless token. No user records, no
  session store. Principals and groups are issuer-qualified (`oidc:v1:<digest>:<claim>`) because a
  subject is unique only within its provider, so changing issuers mints a disjoint population
  rather than silently reassigning grants. The signing key and the deployment's token issuer id
  persist beside the grants, so tokens survive restarts and verify across replicas, and a
  deployment will not accept a token minted by another that happens to share its keyring.
  `rotate_signing_key` mints a successor; with `retire_previous` it stops honouring every token
  signed by an earlier key, which is how a deployment signs all its people out at once — immediately on the replica that
  served it, and within the keyring refresh interval elsewhere.
- **`unauthorized` error code** (401) in the shared taxonomy. `auth: off` never emits it, and the
  default is still `auth: off` — nothing changes for existing deployments.

### Changed

- **Embedding model tarballs are no longer attached to each release.** They are published once
  under their own tag (`models-v1`) and shared by every dolmen version, which keeps ~370 MB of
  identical bytes off every release. Existing download URLs for v0.3.0 and earlier keep working;
  new ones drop the version from the asset name and take the model tag instead, e.g.
  `releases/download/models-v1/dolmen-model-all-MiniLM-L6-v2.tar.gz`. Each release's notes link the
  model tag it was built against.

## v0.3.0

Realtime change feeds, an MCP stdio transport, and an in-process Go library. Five behavior
changes need attention before upgrading.

### Breaking

- **Reads no longer create namespaces.** `list_tables`, `query`, and `changes_since` used to
  answer `200` for a namespace that did not exist, creating its database file as a side effect;
  a mistyped name left `<ns>.db`, `-wal` and `-shm` behind and held their connections. Every read
  now answers `404 not_found` and creates nothing. The write ops (`create_table`, `insert`,
  `update`, `upsert`, `upsert_by_key`, `delete`, `migrate`) still create implicitly on first use.
  If you relied on a read to provision a namespace, call `create_namespace` or a write op instead.
- **The executable moved to `cmd/dolmen`.** Install with
  `go install github.com/lsm/dolmen/cmd/dolmen@v0.3.0`. Released binaries and the container image
  are unaffected.
- **`subscribe` on a missing namespace** is an in-stream `not_found` error instead of a silent
  `200` with an empty stream.
- **`describe_server.usable`** no longer reports `true` when the local embedding model is not yet
  cached. A new `model_cached` field reports the cache state separately. Clients that branched on
  `usable` to decide whether embedding would succeed now get an honest answer, and a different one.
- **Error messages changed** across the decode, idempotency, full-text, and embedding paths. All
  codes and HTTP statuses are unchanged. Only clients matching on message text are affected.

### Added

- **Change feeds.** `changes_since` replays a namespace's durable change log from a cursor;
  `wait_for` long-polls it server-side (default 30s, max 60s) so an agent blocks instead of
  spending tokens on a poll loop; `subscribe` (SSE, `GET /v1/subscribe`) streams replay-then-live
  with keepalive frames and a resume cursor.
- **`read_rows`** fetches rows by id, and **`capabilities`** reports the engine's static surface.
  Twenty-three operations total, up from nineteen, and none were removed. `openapi.json` lists 24
  paths: one `POST /v1/{op}` per operation, plus `GET /v1/subscribe`, which is a stream rather than
  an operation and so has no entry in `tools/list`.
- **MCP over stdio.** `dolmen mcp` serves the same dispatcher on stdin/stdout for hosts that launch
  a subprocess, with a bounded drain on shutdown.
- **Embedded Go library.** `import "github.com/lsm/dolmen"` opens a store in-process with no port
  or subprocess. The embedding provider is optional; `dolmen.Open(dir)` serves every non-vector
  operation.
- **Enum fields** restrict a string field to a closed vocabulary, evolved with `migrate set_enum`.
- **`now()` timestamp defaults** stamped server-side, so idempotent retries can omit the field.
- **Catalog format version** stamped per namespace. The server refuses to start against a data
  directory written by a newer dolmen, naming both versions. Downgrading to v0.2.0 is still safe:
  it ignores the stamp.
- **Sub-path hosting behind a stripping proxy.** The public prefix is recovered from a forwarded
  original URI (nginx `$request_uri` via `X-Original-URI`, plus the Envoy and ingress spellings),
  RFC 7239 `Forwarded` is honored, and the server warns once when it would advertise a base URL no
  proxied client can reach.
- **`-engine` / `DOLMEN_ENGINE`** selects the storage engine. Only `sqlite` is implemented; any
  other value is rejected at startup. This is the seam for future engines, not a preview of one.

### Security

- **A forwarded-header value could inject arbitrary text into the served skill markdown.** The
  sub-path inference added in this release accepted any single-line header value as the public
  prefix, and the skills interpolate that prefix into shell snippets an agent is told to paste and
  run. An unauthenticated request could therefore place attacker-chosen text, including quote-
  breaking shell characters and injected instructions, into the document another agent executes,
  and into the MCP `initialize` instructions. The same path amplified a 1 MB header into a 15 MB
  response. Prefixes are now validated (a conservative character set, no dot or empty segments, and
  length and segment caps), the skills single-quote interpolated URLs, and the skills and OpenAPI
  responses are marked `private, no-store` with a `Vary` so a shared cache cannot serve one
  request's rendering to everyone. The forwarded host and scheme are validated the same way, since
  they reach the same snippets and the OpenAPI `servers` URL, and the PowerShell snippet uses a
  literal-quoted string so it cannot interpolate either. Setting `DOLMEN_BASE_URL` was, and remains,
  a complete mitigation.
- **A JSON number that did not fit its field echoed the caller's literal back in the error.** A
  multi-megabyte literal produced a multi-megabyte error message. The message now names the type
  only.

### Fixed

- A revoked subscription could report a clean end instead of its revocation cause under race
  scheduling.
- A wrongly typed JSON value reported `invalid JSON` and leaked Go-internal type names; it now
  names the field and both types.
- Zero-argument `/v1` operations accept an empty request body instead of answering `415`.
- Full-text errors for bare hyphenated terms teach the quoting fix instead of blaming a missing
  column.
- Idempotency conflicts teach a remediation that does not create duplicates.
- User input can no longer rewrite an error message through the redaction gates.
- A namespace whose catalog is too new is refused before anything is written to it; previously the
  registry tables were recreated inside the newer database before the refusal.
- The Go facade rejects a `nil` record in `Insert`, as the HTTP and MCP surfaces already did.
  Previously it inserted a phantom row that bypassed required-field validation.
- `model_cached` reports `true` once the local model is on disk. The completeness check demanded a
  `config.json` for parameterless modules that are never materialized, so the default model reported
  itself uncached forever and every start logged a spurious download warning.
- `/v1/subscribe` appears in `/v1/openapi.json`, so a client working from that document can discover
  the live stream.
- A read against a missing namespace names the remediation instead of only the namespace.

### Upgrading

Point the new binary at the same data directory. No migration runs, and the catalog stamp is
written on first open of each namespace. Review the read-creates-namespace change first — it is
the one most likely to be load-bearing in existing client code.

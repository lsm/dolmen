# Changelog

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
  Twenty-three operations total, up from nineteen. No operations were removed.
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

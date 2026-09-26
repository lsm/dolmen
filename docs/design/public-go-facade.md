# Public Go façade

Status: proposed design for [#279](https://github.com/lsm/dolmen/issues/279).
This PR settles the library boundary; it does not implement or advertise a usable
Go API yet. The decisions below become the implementation contract when accepted.

## Purpose and architectural boundary

Go CLIs and services should be able to open dolmen in their own process, provision
schemas, write records, and query/search without a port, IPC, or subprocess.
SQLite is the initial backend. This does not implement Postgres or a lakehouse
backend, or change their demand gates in [the engine design](identity-and-engines.md).
The engine's consistency and namespace-confinement obligations remain unchanged.

The public package owns a small, idiomatic application API. It wraps the internal
engine through shared operation logic, rather than exposing `store.Engine` or
serializing requests through `api.Server`. HTTP/MCP and Go calls must use the same
semantic validation, embedding orchestration, and error classification. Wire
parsing, status codes, request IDs, and response redaction remain transport concerns.

The current tree already separates storage from HTTP, but not all operation logic:
`internal/api/ops.go` prepares text vector queries and implements `wait_for`, while
`internal/api/envelope.go` classifies storage errors. Extract the shared pieces
needed by the core; do not duplicate them in a second dispatcher.

## 1. Module and executable

Use package `dolmen` at `github.com/lsm/dolmen`, in the existing module and release
stream. Move root `main.go` and its tests to `cmd/dolmen`. Do not introduce a nested
module or a public backend interface.

The implementation must update Makefile, Dockerfile, CI/release scripts, blackbox
build helpers, and installation/run examples that currently target the root.
The executable remains named `dolmen`; its install path becomes
`github.com/lsm/dolmen/cmd/dolmen`. Publish this command-path change in release notes.
Historical version installation instructions continue to describe their own layout.

Return a concrete `*Store`. Keep its engine private so backend interfaces can evolve
without requiring every library consumer or mock to implement new methods.
Consumers can define the small interfaces they need. No `DB()`, raw SQL connection,
public `Engine`, or backend injection option is included in the first release.

**Amended when PostgreSQL became selectable.** `WithEngineOpener` is a backend
injection option, so this paragraph no longer holds in full. Two of its three
prohibitions do: there is still no `DB()`, no raw SQL connection, and no public
`Engine` — `EngineOpener` names `internal/store.Engine`, which no consumer outside
the module can import, so the option is a seam between first-party packages that
happens to be exported rather than an injection point third parties can reach.

It exists to keep the driver out of everyone else's build. Reaching the PostgreSQL
adapter from the root package put pgx and an embedded WASM parser into the import
graph of every consumer of the module and more than doubled a SQLite-only binary, so
`github.com/lsm/dolmen/postgres` owns the dependency and hands the facade an opener.
The engine stays private, and the reason the original decision gave — that backend
interfaces must evolve without breaking consumers and mocks — is unaffected, because
nothing outside the module can name the interface being passed.

## 2. Curated core

The entry point is `Open(dataDir string, opts ...Option) (*Store, error)`, paired
with `Close() error`. Every operation that performs database or provider work takes
`context.Context` as its first argument. Construction validates options before
filesystem side effects and does not download or load an embedding model.

The first slice exposes these operation families:

| Family | Methods | Required public values |
| --- | --- | --- |
| Namespace lifecycle | `CreateNamespace`, `ListNamespaces`, `DropNamespace` | Names and list options |
| Table lifecycle | `CreateTable`, `ListTables`, `DescribeTable`, `DropTable` | `Field`, `FieldType`, `TableSchema`, create options |
| Writes | `Insert`, `Update`, `Delete`, `UpsertByKey` | Operation-specific options and write results |
| Reads | `GetRows`, `Query` | Query options and `QueryResult` |
| Search | `SearchFulltext`, `SearchVector` | Search options, `VectorQuery`, `SearchResult` |

Use named options structs for filters, arguments, limits, offsets, hidden-column
selection, and idempotency where the corresponding operation supports them. Avoid
copying the engine's long positional signatures. Required coordinates remain
namespace and table strings; do not introduce long-lived table handles that imply
binding to a table incarnation. Each call resolves the current named resource.

Public schema/result types must be nameable without importing `internal` packages.
Keep public definitions deliberate and convert at the boundary; do not alias the
entire internal type surface. Preserve schema versions, embedding identity/dimension,
write IDs/counts/replay state/change ranges, truncation, and skipped-vector metadata
where supported by the operation. Do not expose auth bindings, row scopes, namespace
generations, internal incarnation tokens, or SQLite implementation types.

Records remain `map[string]any`; results retain the engine's typed Go values rather
than taking a JSON round trip. Pin accepted input and returned value types in the
implementation's package documentation and external-package tests, including numeric,
timestamp, JSON, vector, and null values. Match existing operation semantics for
normalization, namespace creation, filters, limits, defaults, and hidden columns.
Search/query results remain bounded, with truncation observable to callers.

Defer migrations, filter-based `Upsert`, schema inference, capability discovery, and
notifications to additive follow-ups. This is a usable create/write/read core, not
an undertaking to export every operation immediately.

Embedded access is trusted local access, equivalent to the current auth-off mode.
It is not an authenticated client of a separately configured server. Do not export
principal/owner overrides or promise row-level authorization in this slice. Future
identity-aware embedding needs its own explicit design.

### Batch relationship

[#267](https://github.com/lsm/dolmen/issues/267) remains the authority for atomic
multi-table writes. Do not invent `Begin`, transaction callbacks, or cross-namespace
transactions here. Its eventual typed same-namespace `Batch` operation can be added
to `*Store`. If it lands before the façade, incorporate its accepted shape; otherwise
it does not block the core. Multiple existing write calls are separate commits.

## 3. Errors shared with transports

Expose `Error`, `ErrorCode`, and category sentinels from the root package. Preserve
the existing taxonomy: `invalid_request`, `not_found`, `query_error`, `conflict`,
`forbidden`, `embedder_unavailable`, `canceled`, `timeout`, and `internal_error`. The forbidden
category is reserved for shared classification; trusted embedded access does not introduce auth.
`timeout` is what an expired context deadline classifies as, on the wire (where the server's
operation limits set the deadline) and in the façade (where the caller's context does), so
`errors.Is(err, dolmen.ErrTimeout)` and `errors.Is(err, context.DeadlineExceeded)` both hold.

`Error` carries a stable code and human-readable message, supports `Unwrap`, and
supports category matching such as `errors.Is(err, dolmen.ErrConflict)`. Use
`errors.As` for structured details such as expected/actual schema versions. Messages
are diagnostic text, not a compatibility or parsing contract. Preserve underlying
causes for in-process diagnostics and standard context cancellation/deadline matching.

Put shared semantic definitions in a dependency-neutral internal package, with
explicit root exports, so neither the engine nor HTTP needs to import the root
façade. Classify failures at their source. In particular, replace the current
`isConflict` message-substring checks with typed conflict causes. Both transports
and the façade consume this classification; the façade must not import the HTTP
error envelope or inherit its logging assumptions.

HTTP retains status mapping, request IDs, and sanitized public messages. Preserve
existing wire behavior during extraction, including existing status distinctions;
changing status policy requires a separate change. Embedded errors retain causes
without referring callers to a server log or requiring `DOLMEN_EMBED_*` configuration.
A failed provider call must be classifiable independently of whether the provider
is built in or supplied by the embedding application.

## 4. Embedding configuration

The initial options are `WithEmbedding(provider)` and `WithChangeRetention(duration)`.
`WithVectorCacheBytes(n)` sizes the SQLite engine's in-memory vector cache (default 512 MiB, the
server's `-vector-cache-size`; 0 disables it). Memory is only used by tables that are vector-searched.
`WithTracerProvider(tp)` records a `dolmen.op <op>` span per facade call (named after the wire
operation, with `dolmen.op.outcome`, `db.namespace` and `dolmen.table`) and an `embeddings` span
per provider call, on the given `trace.TracerProvider`. It never reads or sets OpenTelemetry's global
provider or propagator and does not read `OTEL_*` variables; omitting it leaves tracing off, and a nil
provider is `invalid_request`. The same privacy rule as the server holds: no SQL, filter arguments,
record values or embedded text in spans.
`WithMeterProvider(mp)` records the same metrics as the server on the given `metric.MeterProvider`:
`dolmen.operation.duration` and `dolmen.operations.in_flight` per facade call, and the
`gen_ai.client.*` embedding metrics. Like the tracer option it never touches OpenTelemetry's global
state, omitting it leaves metrics off, and a nil provider is `invalid_request`. Either option may be
given without the other.
Default embedding is disabled. `Open` does not read environment variables, install
signal handlers, replace the global logger, or start a server. CLI environment/flag
interpretation remains in the executable.

The public embedding interface requires `Identity() string`,
`Embed(context.Context, []string) ([][]float32, error)`, and
`EmbedQuery(context.Context, string) ([]float32, error)`. Query and document methods
remain distinct to support asymmetric models such as E5. Identity must describe the
embedding space, including preprocessing, and remain stable for the provider's
lifetime. Reject unusable identity when an operation requires generated embeddings;
raw-vector search does not require a provider.

`WithSecretKey(key []byte)` supplies the 32-byte key that encrypts `secret` fields
(`secret-fields.md`). `Open` rejects a key of any other length with `invalid_request`, copies the
key, and keeps only the derived cipher. Without it, secret fields cannot be created and secret
values cannot be written, the way vector features are refused without `WithEmbedding`; masked
reads still work. `GetRows` and the searches return the mask `"••••"` for a set secret.
`RevealRows(ctx, ns, table, ids, fields)` is `GetRows` with the named secret fields decrypted, and
`SearchOptions.Reveal` does the same for both searches. The facade has no auth, so reveal is always
allowed there; the transports refuse it under `-auth on` until the `reveal` verb ships.
`WithSecretKey(key, retired...)` also takes retired keys, used only to decrypt, and
`RotateSecretKey(ctx, namespace, limit)` re-encrypts one namespace's values under the active key
(limit 0 means no limit; the facade has no operation timeout to bound it) and returns rotated,
remaining and values per key id. Both were cheap because the engine does the work; the facade
covers one namespace per call, and a caller wanting every namespace loops over `ListNamespaces`.

Share the current vector-column/model validation and query-vector validation between
Go and HTTP. Preserve table identity pinning and failed-write atomicity/idempotency.
The application owns an injected provider and its cleanup; closing a store does not
close a provider that may be shared with another store. Providers must support
concurrent calls and honor cancellation where possible.

Built-in provider constructors are a follow-up, not required for the core. Keep the
core free of a mandatory dependency on the local model runtime. Before exposing a
built-in local provider, remove its reliance on process-wide cache mutation:
`internal/embed/local.go` currently sets `REMBED_CACHE`, and later providers reuse
that value. Two embedded stores must be able to select independent cache locations.
Model/cache paths, network behavior, and provider resource ownership must then be
explicit options rather than ambient process configuration.

## 5. Store lifetime and concurrency

A store supports concurrent operation calls. `Close` is terminal and idempotent;
subsequent calls return an `ErrClosed` sentinel (a local lifecycle error, outside the
wire taxonomy). Repeated close calls return the same completion result. The current
internal close implementation only clears its namespace map and can reopen database
connections on later access; this must be fixed before publishing the façade.

Close rejects new work, cancels store-owned background work, and waits for admitted
operations to finish before releasing their resources. An admitted operation may
complete or return a cancellation/closed error; writes must retain existing atomicity.
Close may wait on an application-supplied provider that ignores cancellation. Document
that bound rather than promising forced interruption of arbitrary application code.
Failed construction must clean up acquired resources.

SQLite supports one live store owner per data directory; opening a server and an
embedded store against the same directory is unsupported. For the first slice,
reject duplicate live opens within a process using a canonical-directory ownership
check, release ownership on close/failure, and test symlink aliases. Cross-process
exclusion remains the existing deployment rule, not a newly promised locking feature.
Different directories can be opened independently in the same process.

## 6. Notifications follow-up

Expose a concrete listener with `Next(ctx)` and `Close`, plus an opaque resume cursor.
Prefer a pull interface over a channel: errors, cancellation, cursor advancement,
and backpressure are part of the contract, not side channels. `ChangesSince` and
`WaitFor` belong in this follow-up as well.

Reuse the existing listener state machine and cancellation rules. Today the engine
combines `ChangeReplay.Next` with live callbacks, so the public iterator requires an
adapter with a bounded queue. It must never silently discard changes or advance its
public cursor beyond records delivered to the caller. Preserve explicit terminal
overflow/lifetime errors and retention rules. Closing the listener or store wakes a
blocked `Next`; request cancellation remains distinguishable from terminal shutdown.

Before implementing this follow-up, pin the exact `Next` result shape, single-reader
rule, per-call cancellation behavior, resume behavior after a partial page, and terminal
error precedence against the existing listener tests. This design selects the handle
shape without claiming those details are already implemented by `ChangeReplay`.

## 7. Version policy

Version the Go API with the binary. While on v0, breaking Go API changes occur only
in minor releases; patch releases preserve source and documented behavioral
compatibility. Document breaks and migration steps in release notes. At v1, breaking
changes require a major release and the corresponding Go module major-version path.
New concrete methods and optional settings can be additive. Public result types,
error categories, defaults, and lifecycle behavior are part of the contract.

The root-command move ships in a minor release. This proposal does not weaken the
existing HTTP compatibility requirements in the identity/engine design.

## Delivery and acceptance

After design acceptance, use one core implementation slice, with a stack of reviewable
PRs if necessary. It covers command relocation, public types/methods, shared semantics
and errors, lifecycle fixes, and documentation. Notifications and built-in embedding
constructors remain follow-ups. Do not couple this work to implementing another engine.

Acceptance for the core:

- An external-module example imports only public dolmen packages and provisions a
  namespace/table, inserts, reads, searches, handles a typed error, and closes.
- Conformance compares embedded calls with HTTP/MCP for overlapping operations:
  defaults/coercion, typed results after transport normalization, pagination,
  idempotent retries, conflict/not-found/query errors, embedding identity and failures.
- Focused lifecycle tests cover concurrent operations/close, repeated close, calls
  after close, duplicate directory opens, independent stores, and construction failure.
- Provider tests cover query/document separation, custom-provider failures, disabled
  embedding, cancellation, and ownership. Core imports do not pull in the local runtime.
- Build/install checks exercise `cmd/dolmen`, release and container targets are updated,
  and normal repository test, race, and vulnerability checks pass for implementation.
- Package examples and README installation guidance describe the implemented API,
  supported topology, value types, error handling, and v0 compatibility policy.

This design-only PR needs link/content review and `git diff --check`; it changes no
runtime behavior and does not require rerunning the implementation test suite.

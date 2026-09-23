# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Dolmen is a single static Go binary that gives AI agents a data layer: typed tables, FTS5 full-text search, and cosine vector search over one SQLite file per namespace. It exposes 23 operations over two wire transports, plus seven more under `-auth on` (grants, API keys, `whoami`) and `rotate_signing_key` when native sign-in is configured: HTTP `POST /v1/{op}` and MCP `tools/call` (over HTTP at `/mcp` or over stdio via `dolmen mcp`). An in-process Go library (the root `dolmen` package) exposes a subset of those operations over the same engine, with a few recorded per-surface differences; see the root-package entry under Architecture for what it covers. Module path is `github.com/lsm/dolmen`; the executable lives at `./cmd/dolmen`.

## Commands

```bash
make test                 # go vet ./... && go test ./...
make race                 # go vet + go test -race ./...   (CI runs CGO_ENABLED=1 go test -race ./...)
make build                # CGO_ENABLED=0 static binary -> ./dolmen, version injected via ldflags
make run                  # go run ./cmd/dolmen -addr 127.0.0.1:8790 -data ./data
make vulncheck            # govulncheck ./... (needs govulncheck on PATH)
```

Single package / single test:

```bash
go test ./internal/store -run TestName
go test . -run TestName                    # root package (public Go facade)
go test ./internal/conformance             # contract suite over HTTP + MCP + embedded facade
go test ./internal/blackbox                # builds the binary and drives it as a subprocess
```

Other checks CI runs on every pull request that `make test` does not:

```bash
# Zero-comments gate (fails CI if any Go comment exists; see Conventions)
go run github.com/lsm/nocomment-for-agents/go@016ad219e84b9b78ca66ff1b66a7729218030c12 --check

# examples/basic is a separate module (replace => ../..); CI builds, vets, and runs it
(cd examples/basic && go build ./... && go vet ./... && go run .)

# Vulnerability gate (.github/workflows/vuln.yml); install: go install golang.org/x/vuln/cmd/govulncheck@latest
make vulncheck
```

Opt-in test that downloads real embedding models (skipped otherwise):

```bash
DOLMEN_TEST_EMBED_LOCAL=1 go test ./internal/embed -run Live
```

Notes on the test tiers:
- **macOS quirk:** three symlink tests in the root package (`TestOpenDotDotThroughSymlinkSharesOwnership`, `TestOpenResolvesRelativeSymlinkTargetInPlace`, `TestOpenResolvesRelativeTargetThroughSymlinkAndDotDot`) fail when `TMPDIR` is the default `/var/folders/...`, because that path is itself a symlink to `/private/var/...` and the tests compare against the unresolved `t.TempDir()`. Linux CI is unaffected. Locally, run with a physically resolved temp dir: `TMPDIR="$(realpath "$TMPDIR")" go test ./...`.
- `internal/blackbox` is a `*_test.go`-only package. `TestMain` builds `./cmd/dolmen`, boots one server, and stages 01–11 share global state in file order. Run the package as a whole; `-run TestStage05...` alone will not work. A guard test forbids importing `internal/` or `skill` there.
- `internal/conformance` uses an in-process `httptest` server plus a fake embedding provider. Its `harnessMode` picks the auth setup per test: `off`, `admin-key`, `gateway`, `gateway-no-key`, or `keys-only`. The OIDC tests attach the sign-in source to an auth-on harness against a local issuer stub.
- Releases: pushing any tag matching `v*` runs `.github/workflows/release.yml`, which publishes cross-compiled binaries, an SBOM, a GHCR image, and a GitHub release. Only an exact `vX.Y.Z` tag becomes a final release tagged `latest`; any other `v...` tag (an RC, for example) still publishes all of that as a prerelease. Do not push a `v` tag unless a release is intended.
- Embedding models are released separately. `.github/workflows/release-models.yml` runs on a `models-v*` tag and publishes the `cmd/pack-model` tarballs under that tag alone; every dolmen release links to the tag named by `MODEL_RELEASE` in the Makefile instead of carrying its own copy. Cut a new one only when a revision in `EMBED_MODELS` is re-pinned, and bump `MODEL_RELEASE` in the same commit — the models workflow refuses a tag that does not match it, and the release workflow refuses to build when no release is published under it, so push the `models-v*` tag before the next `v*` tag.

## Conventions that are enforced or expected

- **No comments in Go source.** CI runs a zero-comments check; the tree has none. Do not add doc comments, even on exported identifiers, and do not add explanatory inline comments. `//go:embed`-style directives are allowed. `go/allowlist.txt` is the gate's per-file allowlist and is empty; keep it that way. Explanations belong in `README.md`, `docs/design/`, or the commit message.
- **Commit subjects** follow `Dolmen#<issue> - <summary>`; some carry a trailing `(#PR)` from squash-merge.
- **Behavior-changing PRs update `internal/conformance` in the same PR.** The suite pins transport parity between `/v1` and MCP, the error contract, the limits table, typed-read coercion, write semantics, search invariants, and migration guards.
- **Concurrency-touching changes must pass `make race`**, not only `make test` (notifications, listen/drain, drop cascades).
- **Pure Go, `CGO_ENABLED=0`.** SQLite is `modernc.org/sqlite`; embeddings are `rembed`. Do not introduce cgo dependencies.
- **Design authority.** `docs/design/identity-and-engines.md` is the spec for the auth / multi-tenancy / pluggable-engine epic (#159) and `implementation-plan.md` is its slice order. Deviating from the spec requires editing the spec first, in the same PR. `public-go-facade.md`, `facade-input-matrix.md`, and `numeric-fidelity-matrix.md` record the settled decisions for the Go library.
- **Errors teach.** Messages name the remediation (which field, which limit, what to call instead) and are stable enough that tests pin them. User input must never be able to rewrite an error message, and file paths are redacted before leaving the API layer (`internal/api/envelope.go`).

## Architecture

Request flow: transport → `internal/api` op table → `store.Engine` (SQLite). Most storage-backed `OpDef.Func` bodies call the engine directly and reach for `internal/ops` helpers where needed (namespace ensure, vector-query preparation, error classification). Three kinds of exception are worth knowing before you trace or add an operation:

- **Pure operations** never touch the engine. `describe_server` reads only the embedding provider, and `infer_schema` only calls `schema.InferSchema`.
- **The change feed** goes through an API-layer helper. Both `changes_since` and `wait_for` call `runChangesSince`, which is what actually invokes `engine.ChangesSince`. `wait_for` wraps that in its own polling loop with the deadline and timeout handling living in the API layer, not the engine. Change either operation's feed semantics in the helper so both stay in step.
- **SSE** is not an operation at all. `/v1/subscribe` is a plain handler in `sse.go` that calls `engine.Listen` directly. The Go facade skips the transport and op table and calls the engine directly too, using the same `internal/ops` helpers. There is no shared operation layer that every call passes through; parity between surfaces is enforced by the conformance suite, not by a common code path.

**`cmd/dolmen`** parses flags and env (`loadConfig`), opens the store, builds the embed provider, then `api.New` and `mcp.New`. Plain `dolmen` serves HTTP; `dolmen mcp` serves the same MCP dispatcher over stdio with logs on stderr. `-prefix` mounts everything under a sub-path.

**`internal/api`** is the HTTP transport and the single source of truth for operations. `Ops` in `internal/api/ops.go` is a `map[string]OpDef`; each entry carries `Description`, `InputSchema`, `OutputSchema`, and `Func`. Everything else derives from it: `/v1/{op}` routing (`server.go`), `/v1/openapi.json` (`openapi.go`), and MCP `tools/list` / `tools/call`. To add or change an operation, edit its `OpDef` (schemas included), add its entry to `toolAnnotations` in `internal/mcp/server.go`, give it the verbs it requires in `authRules` (`internal/api/authz.go`, which a test holds complete), and update the conformance suite. Operations that exist only under `-auth on` live in `authOps` (`internal/api/ops_auth.go`). One override to know about: `outputSchemas` in `openapi.go` holds richer output schemas for 17 operations, and that file's `init()` replaces the matching `OpDef.OutputSchema` fields at startup. For those operations, editing the literal in `ops.go` has no effect on what OpenAPI or MCP advertise; edit the `outputSchemas` entry instead. Tests in `internal/mcp` fail if the tool list, annotations, and `Ops` drift apart. `envelope.go` maps engine errors to HTTP status and error code (`wrapStoreErr`, `statusFor`). `sse.go` serves `/v1/subscribe`.

**`internal/mcp`** wraps `api.Server` in JSON-RPC 2.0 for `/mcp` and stdio. Successful tool results go in `structuredContent`; `content` stays empty. `drain.go` implements the stdio shutdown grace period.

**`internal/ops`** holds the logic shared by both transports and the Go facade: the embedding adapter, `EnsureNamespace`, `PrepareVectorQuery`, name normalization, and `Classify(err) derr.Code`.

**`internal/derr`** is the error taxonomy every surface agrees on: `invalid_request`, `not_found`, `query_error`, `conflict`, `unauthorized`, `forbidden`, `embedder_unavailable`, `canceled`, `timeout`, `internal_error`. An operation past its server-side limit (`-op-timeout`, `-migrate-timeout`, set per operation in `api.Server.Dispatch`) and any expired context deadline classify as `timeout`. The root package re-exports these as `dolmen.ErrX` sentinels matched with `errors.Is`.

**`internal/store`** defines the `Engine` interface (`engine.go`) and its SQLite implementation. One namespace is one file `<data>/<ns>.db` in WAL mode with a single writer connection and a read-only pool; read-only SQL runs on a `mode=ro` connection with a SELECT/WITH allowlist. FTS5 shadow tables back full-text fields; vectors are float32 blobs scanned brute-force in Go. Each file also holds a schema registry, migration log, idempotency-key table, and a durable change log that feeds `changes_since`, `wait_for`, and SSE (`listen_*.go`, `notify.go`). Typed value coercion and decoding delegate to `internal/value`: `coerceValue` in `insert.go` preserves adapter default markers and `decodeValue` in `typed.go` applies declared field types. The `Engine` methods carry parameters for the auth epic that look like dead scaffolding under the current `auth: off` behavior but are not: `AuthBinding`, `RowScope`, `Incarnation`, `nsGen [16]byte`, `WriteOpts.Owner` / `TableWideRead`, `TableOpts.RowAccess`, and `Listen`'s `liveAuthz` callback. Several are load-bearing. Under `-auth on`, `internal/api/scope.go` and the op table fill `RowScope`, `WriteOpts.Owner` / `TableWideRead`, and `TableOpts.RowAccess` with the caller's reach. `Incarnation.Version` carries `expected_version` for `migrate` and its dry run, and the engine turns a mismatch into a conflict. `Incarnation.Table` is populated by `TableState` and compared in `listen_live.go` so a subscription is not admitted across table lifetimes. The `liveAuthz` callback, with `RowScope`, drives that same admission check. `AuthBinding` is still nil at every call site. Treat the whole set as required state: read `internal/store/engine.go` and `docs/design/identity-and-engines.md` before touching any of it, and do not remove a member because callers appear to pass a zero value.

**`internal/schema`** owns field types, the table and field name grammar and reserved names, enum/default rules, migration `Change` ops, and the vector blob encoding. It defines and validates schema metadata only. Value coercion lives in `internal/value`; the namespace grammar remains in `internal/store`: segment pattern and depth in `store.go`, path validation in `lifecycle.go`.

**`internal/embed`** defines `Provider` (`Name`, `Identity`, `ModelName`, `Embed`, `EmbedQuery`) with `none`, `local` (rembed, in-process, model cached under `<data>/models`), and `openai` implementations. The identity string pins a vectorized table to its embedding space. Non-instruct e5-family models get `query:` / `passage:` prefixes automatically; models whose name contains `instruct` are deliberately excluded, so their instruction-specific prompting is the caller's job.

**Root package `dolmen`** is the public Go facade: `Open`/`Close`, namespace and table lifecycle, `Insert`/`Update`/`Delete`/`UpsertByKey`, `GetRows`/`Query`, `SearchFulltext`/`SearchVector`. `Open` never reads env or loads a model. The embedding provider is optional: `Open(dataDir)` with no options is a supported deployment that serves every non-vector operation, and only work needing an embedding space is refused, with an error saying to pass `WithEmbedding`. Supply a provider when you need `vectorize` fields or text vector search; do not invent a dummy one to satisfy `Open`. One live `Store` per data directory per process. It shares the engine's semantics with the transports, but the surfaces diverge in several recorded, by-design places. Two examples, not an exhaustive list: a malformed table name is `invalid_request` on the facade and `not_found` on the wire, and an explicit delete `limit` of 0 falls back to the default on the facade while the wire rejects it. Numeric handling has its own set, such as typed integers above the int64 maximum being rejected on the facade while the wire's `json.Number` token path accepts them with rounding. Two design docs are the authority on which divergences are intentional: `docs/design/facade-input-matrix.md` for input validation and `docs/design/numeric-fidelity-matrix.md` for number handling and read-back types. `internal/conformance/embedded_parity_test.go` pins both the agreements and many of the divergences. Check both matrices before changing facade behavior, and do not "fix" a recorded divergence without updating the matrix and the test in the same PR.

**`skill/`** embeds `dolmen.md` and `dolmen-admin.md` as Go templates served at `/skills/*`. They are part of the served contract and have tests; keep them in sync with behavior changes.

## Configuration reference

Flags and env vars are documented in the README "Configuration" table and in `dolmen -help`. Defaults: `127.0.0.1:8790`, data dir `data`, embedding provider `local`, `-auth off`. Auth is off by default, which is why the server binds to loopback; the README's Authentication section covers `-auth on` and its four identity sources.

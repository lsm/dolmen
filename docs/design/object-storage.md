# Object storage

Tracking issue: #613. This is a proposal. The decisions marked **Open** need an answer before slice 1
starts, because each one fixes part of the public contract.

## Problem

Agents keep rows that describe binary objects: the recording behind a transcript, the PDF behind an
extracted record, the image behind a caption. Today the only place for the bytes is a `json` or
`string` value, which caps them at the 32 MiB request body, pushes them through JSON as base64,
loads them whole into memory on every read, and puts them in the agent's context when an agent reads
the row. Agents rarely need the bytes themselves; they need to attach a file to a row, find it again,
and hand it to the tool that processes it.

## Shape

- A **`file` field type**. The row holds a small reference; the bytes live in a blob store beside
  the namespace. Every existing read, search, `query`, filter and change-feed path keeps working on
  the reference.
- **Bytes move over plain HTTP**, never through `/v1/{op}` or MCP: a streaming upload endpoint and a
  streaming download endpoint. Operations only ever carry references.
- **Content addressed by SHA-256.** A blob is stored once per namespace however many rows reference
  it, a retried upload stores nothing new, and the hash is the integrity check and the `ETag`.
- **A pluggable blob store** behind one small interface: a local directory first, an S3-compatible
  store later. Both are pure Go, so the build stays `CGO_ENABLED=0`.
- **Garbage collected by `vacuum`**, by mark and sweep over the file columns, never by reference
  counts.

Out of scope: transcoding, thumbnails, transcription or text extraction, encryption of blobs at rest,
and full-text or vector search over file contents. Each is a separate tool that reads the file
through the download endpoint and writes its result back as ordinary fields.

## The `file` type

**Declaring it.** `{"name": "recording", "type": "file"}`, with two optional constraints:

- `max_bytes`: the largest blob the field accepts, at most the server's `-max-file-size`.
- `media_types`: accepted media types, exact (`application/pdf`) or a top-level wildcard
  (`audio/*`).

`fulltext`, `vectorize`, `enum`, `shape` and `default` are refused on a file field, at
`create_table` and through every `migrate` op, as they are on `secret`. A file field cannot be an
`upsert_by_key` key. `required` is allowed. `migrate` gains `set_file_constraints` to change
`max_bytes` and `media_types`. Like `set_enum` it refuses when stored references violate the new
constraint, reporting how many rows do, and like `set_shape` it names their ids, withholding both
from a row-scoped caller as those ops do. Because it reads rows, it joins `Change.ReadsRows` and needs
the `read` verb under `-auth on`.

**The value on read.** A non-null file value reads back as an object:

```json
{"sha256": "9f86d0…", "size": 48213, "media_type": "audio/ogg", "name": "standup.ogg"}
```

`name` is the filename given at upload or at write, `null` when neither gave one. It is validated
on both paths: 1 to 255 bytes of UTF-8, no control characters, no `/` or `\`; otherwise
`invalid_request` naming the rule. `size` and
`media_type` come from the blob and are never caller-supplied at write time. Every read path returns
the same object: `read_rows`, both searches, `query` (by result-column label, as for every declared
type), and the facade.

**The value on write.** A write never names a hash. It names an upload:

```json
{"recording": {"upload": "up_3Jq…", "name": "standup.ogg"}}
```

The upload id comes from the upload endpoint and is bound to the namespace, the table and field it
was uploaded for, and (under `-auth on`) the principal that uploaded it. `name` is optional and
overrides the upload's filename. `null` clears the field. Writing back the object a read returned is
refused with an error that says to upload the bytes and pass the upload id instead.

Why not accept a hash: content addressing turns a hash into a capability. If a write could attach a
hash, any caller who knows or guesses one, for example the hash of a well-known document or one seen
in a log, could attach a blob it never had and then download it through a row it can read. Binding the
write to an upload the caller made closes that: you can only attach bytes you sent.

**Storage in the row.** The column holds the canonical reference JSON (`sha256` and `name`); `size`
and `media_type` are joined from the namespace's blob table on read. SQLite stores it as TEXT,
PostgreSQL as `jsonb`, the lakehouse as a Parquet string, so `query` and filters can read the hash
with each dialect's JSON accessor (`recording->>'sha256'`, `json_extract(recording, '$.sha256')`).

## Endpoints

Both are plain handlers beside `/v1/subscribe`, at an exact `/v1/files` pattern that `ServeMux` prefers over the `/v1/`
subtree, honoring
`-prefix`, authentication, and the request-id and tracing middleware. A large file outlasts the
server's absolute `-read-timeout` and `-write-timeout` deadlines, so, as `subscribe` already does for
its frames, both handlers re-arm the connection deadline through `http.ResponseController` before
each chunk: the upload's read deadline and the download's write deadline become idle timeouts of
that length, and `-op-timeout` bounds only the upload's final commit. Because the server arms its
write deadline when it reads the request headers, the upload re-arms its write deadline after the
commit and before answering, as every other response path does, so a long upload can still deliver
its `201` and upload id. `files` becomes a reserved
operation name: an operation by that name would be shadowed over HTTP yet still callable through MCP
`tools/call`, breaking transport parity.

**Upload.** `PUT /v1/files?namespace=<ns>&table=<t>&field=<f>[&name=<filename>]`

- The body is the raw bytes. `Content-Type` is the declared media type; without one the server
  sniffs the first 512 bytes. `media_types` filters on that declared type. It is not a content
  check: a caller can declare `application/pdf` over any bytes, and sniffing cannot settle it either,
  since it recognizes few formats. What keeps a mislabeled file harmless is the download's headers. `Content-Length` is optional; chunked bodies are accepted.
- An optional `X-Dolmen-Sha256` header carries the expected hash; a mismatch refuses the upload and
  stores nothing.
- The server streams the body to a temporary file while hashing it, enforces the field's
  `max_bytes` and `media_types` and `-max-file-size` as it goes, and fsyncs it. Only then does it
  take the namespace's writer and, in one write transaction, record the upload: if a
  `_dolmen_blobs` row for the hash exists the temporary file is discarded, otherwise the file is
  renamed into place and the row inserted; then the `_dolmen_uploads` row is inserted. Streaming
  holds no lock; only this short commit does.
- It answers `201` with `{"upload": "up_…", "sha256", "size", "media_type", "name", "expires_at"}`.
- Under `-auth on` it needs `create` or `update` on the table: the verbs of the writes that can use
  the upload.

An upload id is single-namespace and expires after `-upload-ttl` (default `24h`). It can be used by
any number of writes until it expires, so a retried write that replays through its idempotency key
still finds it. The namespace and table must exist; the upload does not create them.

A write re-checks the blob's size and media type against the field's current `max_bytes` and
`media_types`, from `_dolmen_blobs`, so an upload made before `set_file_constraints` tightened them
cannot attach afterwards.

**Download.** `GET /v1/files?namespace=<ns>&table=<t>&id=<row id>&field=<f>`

- Addressed by row, not by hash, so authorization is exactly `read_rows` on that row: the `read`
  verb, or own-row access on a `row_access` table. A row the caller cannot see answers `404`, as
  `read_rows` would leave it out.
- Answers with the bytes through `http.ServeContent`, so `Range`, `If-None-Match` (the `ETag` is the
  quoted hash) and `HEAD` work.
- Always sets `Content-Type` to the stored media type, `X-Content-Type-Options: nosniff`,
  `Content-Disposition: attachment` with the stored name, encoded per RFC 6266: an ASCII
  `filename` with quotes and non-ASCII replaced by `_`, plus `filename*` in RFC 5987 UTF-8 form, and `Content-Security-Policy: sandbox`.
  Dolmen serves its sign-in and token pages on the same origin, so an uploaded HTML or SVG file must
  never render as a page there, where it could script or redress those pages and the API.

**Errors.** Both endpoints answer failures with the ordinary error envelope and codes:

| Case | Status | Code |
|---|---|---|
| Missing or unknown namespace, table, field, row, or a null file value | `404` | `not_found` |
| Field is not a `file` field | `400` | `invalid_request`, naming the field's type |
| Body over the field's `max_bytes` or `-max-file-size` | `413` | `invalid_request`, naming the limit |
| Media type not in `media_types` | `415` | `invalid_request`, naming the accepted types |
| `X-Dolmen-Sha256` mismatch | `400` | `invalid_request`, naming both hashes |
| Disk full | `507` | as `-max-namespace-size` refusals today |
| Blob store unreachable (S3, slice 3) | `503` | **Open**: a new `storage_unavailable`, following `sql_engine_unavailable` |

A write naming an unknown, expired, foreign-namespace or other principal's upload is
`invalid_request`, saying to upload the bytes again; it never says which of those it was, so it does
not confirm that someone else's upload exists.

## MCP and the Go facade

MCP tools never carry bytes. An MCP client that can make HTTP requests uses the two endpoints with the
same credentials. **Open:** whether to add an `upload_file` tool taking base64 `content` capped at a
few MiB, for MCP clients that cannot make HTTP requests, and a `file_url` tool that returns a
short-lived signed download URL (slice 3) the agent can hand to a tool that holds no credentials.

The facade gets `PutFile(ctx, namespace, table, field string, r io.Reader, opts PutFileOptions)
(Upload, error)` and `OpenFile(ctx, namespace, table string, id int64, field string) (io.ReadSeekCloser,
FileInfo, error)`, over the same blob store and the same rules. The facade has no auth, so upload ids
there only carry the namespace, table and field binding.

## Blob store

```go
type BlobStore interface {
	Create(ctx context.Context, ns string) (BlobWriter, error)
	Open(ctx context.Context, ns, sha256 string) (Blob, error)
	Delete(ctx context.Context, ns, sha256 string) error
	List(ctx context.Context, ns string, fn func(sha256 string, modified time.Time) error) error
	DropNamespace(ctx context.Context, ns string) error
}
```

`BlobWriter` hashes as it writes and commits under its hash; `Blob` is an `io.ReadSeekCloser` with
its size. The engine never sees bytes. A new `internal/files` package owns the interface, the local
implementation, the endpoints' streaming and the sweep; each engine owns only the file column, the
upload and blob tables, and the reference scan the sweep needs.

**Local.** `<data>/<ns>.files/<first two hex>/<sha256>`, written to `<ns>.files/tmp/` and renamed.
SQLite and the lakehouse put it beside the namespace's `.db` or `.lakehouse/` directory.

**PostgreSQL.** Rows live in the database but the local store is on one server's disk, so two servers
on one catalog would each see half the blobs. **Open:** refuse `file` fields on PostgreSQL until the
S3 store exists (slice 3), or allow the local store with a documented single-server restriction.

**Metadata.** Each namespace gets `_dolmen_blobs` (`sha256` primary key, `size`, `media_type`,
`created_at`) and `_dolmen_uploads` (`id`, `sha256`, `table_name`, `field_name`, `principal`, `name`,
`expires_at`). The blob file is written before its `_dolmen_blobs` row, so a crash leaves at worst an
unreferenced file, which the sweep removes.

## Garbage collection

`vacuum` gains a sweep. It marks every hash referenced by a file column in any table of the
namespace or by an unexpired upload, then deletes each unmarked blob and its `_dolmen_blobs` row.
The sweep runs holding the namespace's writer, as `vacuum` already does, so no write and no upload
commits during it. A write resolves its upload id inside its own write transaction and, in that same transaction,
requires the upload to be unexpired and its `_dolmen_blobs` row to exist. A write that commits before
the sweep is marked by the reference it stored; a write that commits after it finds the upload expired
or the blob gone and is refused with the ordinary upload-again error. An upload is ordered the same
way: one that commits before the sweep is marked by its unexpired upload row, and one that commits
after it finds no `_dolmen_blobs` row and stores its own file. Neither order leaves a row pointing
at a deleted blob.

The same pass deletes expired `_dolmen_uploads` rows, which no write can use any more, and blob files
that have no `_dolmen_blobs` row, left by a crash between an upload's rename and its commit; since
renames happen only under the writer, no live upload has such a file. Temporary files of uploads
still streaming live under `tmp/` and are deleted only when older than `-upload-ttl`.

Reference counts were rejected: every write path (insert, update, upsert, `upsert_by_key`,
`batch` with its rollback, `drop_field`, `drop_table`, the lakehouse's replayed commit log) would have
to keep them exact, and one missed decrement leaks a blob forever while one extra deletes a blob a row
still references. A full scan is only paid at `vacuum`, which already rewrites the namespace.

`drop_table` and `drop_field` leave their blobs for the next sweep. `drop_namespace` removes the
whole blob directory. `vacuum`'s result gains `files_removed` and `file_bytes_removed`.

## Limits and accounting

| Flag | Default | Meaning |
|---|---|---|
| `-max-file-size` | `1GiB` | Largest single upload; also the ceiling for a field's `max_bytes` |
| `-upload-ttl` | `24h` | How long an upload id stays usable, and the sweep's grace period |
| `-files-store` | `local` | Blob store; `s3` arrives in slice 3 |

The 32 MiB request body limit does not apply to the upload endpoint. **Open:** whether blobs count
toward `-max-namespace-size`. Counting them is what an operator capping disk wants; it also means a
large upload can push a namespace past its cap and refuse every later row write.

## Change feed, backup, auth audit

Change records carry ids, never values, so attaching or replacing a file is an ordinary `update`
change. `dolmen backup` must copy the blob directory and list every blob's hash in its manifest, and
`restore` must verify them; until it does, backup refuses a data directory that holds blobs rather
than silently omitting them. No new verb: upload is a write and download is a read.

## Conformance

Pinned on every engine that accepts `file` fields: the declaration refusals, the read-back object,
the write-by-upload rule and its refusals (raw hash, read-back object, expired, foreign namespace,
other principal), upload and download over HTTP with `Range` and `ETag`, the download's safety
headers, row-scoped download authorization, and that `vacuum` removes exactly the unreferenced blobs
past the grace period. MCP parity is that both transports read and write the same reference.

## Slices

Each one a PR from main.

1. This doc, then the `file` type, upload and download endpoints, the local store, the sweep in
   `vacuum`, the facade methods, skills, README and conformance, on SQLite. `dolmen backup` refuses
   a data directory holding any blob directory, naming slice 4, so no backup silently omits blobs.
2. Lakehouse parity, and PostgreSQL per the open question above.
3. The S3-compatible store, signed download URLs, and `storage_unavailable` if adopted.
4. `dolmen backup` and `restore` with blobs.

## Open decisions

1. A new `storage_unavailable` code (`503`) for an unreachable blob store, or reuse `internal_error`.
2. `upload_file` (base64, capped) and `file_url` MCP tools, or HTTP endpoints only.
3. PostgreSQL: refuse `file` until the S3 store exists, or allow the local store on a single server.
4. Whether blobs count toward `-max-namespace-size`.
5. Whether a write may copy a reference from a row the caller can read (`{"copy_from": {"table",
   "id", "field"}}`) so reusing a file does not need a re-upload. It is safe, because the caller
   could already download those bytes, but it adds a write shape.

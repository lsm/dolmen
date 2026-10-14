# Object storage

Tracking issue: #613. The five contract decisions are settled; the last section records them.

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
`invalid_request` naming the rule. `size` comes from the blob and `media_type` from the upload;
neither is caller-supplied at write time. The media type belongs to the reference, not the blob:
identical bytes uploaded as `audio/ogg` for one field and `application/octet-stream` for another
are one blob with two references, each keeping its own type. Every read path returns
the same object: `read_rows`, both searches, `query` (by result-column label, as for every declared
type), and the facade.

**The value on write.** A write never names a hash. It names an upload:

```json
{"recording": {"upload": "up_3Jq…", "name": "standup.ogg"}}
```

The upload id comes from the upload endpoint and is bound to the namespace, the table and field it
was uploaded for, and (under `-auth on`) the principal that uploaded it. `name` is optional and
overrides the upload's filename. `null` clears the field. Writing back the object a read returned is
refused with an error that says to upload the bytes, or to use `copy_from`, instead.

To reuse a file another row already holds, a write names that row instead of an upload (slice 2):

```json
{"recording": {"copy_from": {"table": "meetings", "id": 42, "field": "recording"}, "name": "q3.ogg"}}
```

The source must be a row the caller could read with `read_rows` in the same namespace: the `read`
verb, or own-row access on a `row_access` table; any other row, a missing one, or a null file value
is the same `invalid_request`, so it does not confirm that a row exists. The reference copies the
source's `sha256`, `media_type` and `name` (`name` here overrides it) and is re-checked against the
target field's `max_bytes` and `media_types`. It resolves inside the write transaction under the
namespace writer, where the source row's reference keeps the blob marked, so no sweep can collect it
in between. This is safe because the caller could already download those bytes; it saves the upload
bandwidth, not any authority.

Why not accept a hash: content addressing turns a hash into a capability. If a write could attach a
hash, any caller who knows or guesses one, for example the hash of a well-known document or one seen
in a log, could attach a blob it never had and then download it through a row it can read. Binding the
write to an upload the caller made closes that: you can only attach bytes you sent.

**Storage in the row.** The column holds the canonical reference JSON (`sha256`, `media_type` and
`name`); `size` is joined from the namespace's blob table on read. SQLite stores it as TEXT,
PostgreSQL as `text` like every `json` field there, the lakehouse as a Parquet string, so `query` and
filters can read the hash with each dialect's JSON accessor (`json_extract(recording, '$.sha256')`
on SQLite and DuckDB, `recording::jsonb->>'sha256'` on PostgreSQL).

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
  `_dolmen_blobs` row for the hash exists and its file is present the temporary file is
  discarded; if the row exists but the file is missing, the temporary file is renamed into place,
  healing it; otherwise the file is renamed into place and the row inserted; then the `_dolmen_uploads` row is inserted. Streaming
  holds no lock; only this short commit does.
- It answers `201` with `{"upload": "up_…", "sha256", "size", "media_type", "name", "expires_at"}`.
- Under `-auth on` it needs `create` or `update` on the table: the verbs of the writes that can use
  the upload.

An upload id is single-namespace and expires after `-upload-ttl` (default `24h`). It can be used by
any number of writes until it expires, so a retried write that replays through its idempotency key
still finds it. The namespace and table must exist; the upload does not create them.

A write re-checks the blob's size and the upload's media type against the field's current
`max_bytes` and `media_types`, so an upload made before `set_file_constraints` tightened them
cannot attach afterwards.

**Download.** `GET /v1/files?namespace=<ns>&table=<t>&id=<row id>&field=<f>`

- Addressed by row, not by hash, so authorization is exactly `read_rows` on that row: the `read`
  verb, or own-row access on a `row_access` table. A row the caller cannot see answers `404`, as
  `read_rows` would leave it out.
- Answers with the bytes through `http.ServeContent`, so `Range`, `If-None-Match` (the `ETag` is the
  quoted hash) and `HEAD` work.
- Always sets `Content-Type` to the row's media type, `X-Content-Type-Options: nosniff`,
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
| Over `-max-namespace-files-size` | `507` | `internal_error`, as `-max-namespace-size` refusals today, naming the flag, the namespace's current file bytes and the cap |
| Blob store unreachable (S3, slice 3) | `503` | `storage_unavailable`, a new code following `sql_engine_unavailable` |

A write naming an unknown, expired, foreign-namespace or other principal's upload is
`invalid_request`, saying to upload the bytes again; it never says which of those it was, so it does
not confirm that someone else's upload exists.

## MCP and the Go facade

MCP tools never carry bytes. An MCP client that can make HTTP requests uses the two endpoints with the
same credentials. Slice 1 adds `PUT` to the methods the origin guard allows in its CORS preflight
(today `GET, POST, OPTIONS`), and `X-Dolmen-Sha256` to its allowed headers, so a browser-based client
on an allowed origin can upload as well as download.

There is no `upload_file` tool: bytes never pass through an agent's context. Slice 3 adds one
operation, `file_url` (`namespace`, `table`, `id`, `field`, optional `ttl_seconds`, default 900, at
most 3600), which returns `{"url", "expires_at"}`: a short-lived download URL the agent can hand to a
tool that holds no credentials. It needs the same reach as the download endpoint. On the S3 store
the URL is the store's own presigned `GET`, with `response-content-type` and
`response-content-disposition` set so the bytes arrive with the same type and as an attachment. On
the local store it is the download endpoint with `exp` and `sig` query parameters: an HMAC-SHA256
over the namespace, table, row id, field, hash and expiry, under a key generated on first use and
kept at `<data>/files.key` (mode `0600`). The signature binds the hash, so replacing the row's file
invalidates every URL issued for the old one.

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
on one catalog would each see half the blobs, and nothing in the engine can tell that a second
server exists. So until the S3 store exists (slice 3), the postgres engine refuses a `file` field at
`create_table` and `add_field` with `invalid_request` saying file fields need `-files-store s3` on
PostgreSQL. From slice 3 it accepts them only with the S3 store configured.

**Metadata.** Each namespace gets `_dolmen_blobs` (`sha256` primary key, `size`,
`created_at`) and `_dolmen_uploads` (`id`, `sha256`, `table_name`, `field_name`, `principal`, `name`,
`media_type`, `expires_at`). The blob file is written before its `_dolmen_blobs` row, so a crash leaves at worst an
unreferenced file, which the sweep removes.

## Garbage collection

`vacuum` gains a sweep. It marks every hash referenced by a file column in any table of the
namespace or by an unexpired upload, then deletes each unmarked blob: it commits the `_dolmen_blobs` row deletions first and only
then unlinks the files, all while still holding the writer. A crash between the two leaves files
without rows, the residue the next sweep already removes, never a row whose file is gone.
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
| `-max-namespace-files-size` | `0` | Largest total of one namespace's stored blobs, as bytes or with `KiB`/`MiB`/`GiB`/`TiB`; `0` is unbounded |

The 32 MiB request body limit does not apply to the upload endpoint. Blobs do not count toward
`-max-namespace-size`, which keeps bounding the namespace's rows; `-max-namespace-files-size` bounds
its blobs separately, so a large upload can never refuse the namespace's row writes. The total is
the sum of `_dolmen_blobs.size`, each blob counted once however many rows reference it. An upload
stops streaming as soon as the current total plus the bytes received would pass the cap, and its
commit checks again under the writer, so concurrent uploads cannot overshoot it together. The
server cannot know an upload's hash before it has read the body, so an upload of bytes already
stored is accepted at its commit, where it adds nothing, but could be stopped mid-stream when the
namespace is near its cap. An upload that sends `X-Dolmen-Sha256` naming a stored hash skips the
streaming check, is verified against that hash, and is accepted at commit. Each namespace entry in
`list_namespaces` gains `file_bytes`.

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
   `-max-namespace-files-size` and the postgres refusal ship here too.
2. Lakehouse parity, and `copy_from` on both engines.
3. The S3-compatible store, `storage_unavailable`, the `file_url` operation, and `file` fields on
   PostgreSQL over the S3 store.
4. `dolmen backup` and `restore` with blobs.

## Decisions

Settled on 2026-10-14.

1. **`storage_unavailable` (`503`)** for an unreachable blob store, so an agent can tell a transient
   outage it may retry from a server fault, and operators can alert on it separately. It is wired
   through `derr`, the API envelope, the OpenAPI error enum, a `dolmen.ErrStorageUnavailable` facade
   sentinel, the skills and the README, as `sql_engine_unavailable` was.
2. **`file_url` only.** No MCP tool carries bytes; uploads go over HTTP.
3. **PostgreSQL refuses `file` fields until the S3 store exists.** A local store there would split the
   blobs between servers silently.
4. **A separate `-max-namespace-files-size`.** Blobs stay out of `-max-namespace-size`, so a large
   file cannot block row writes.
5. **`copy_from` is allowed**, in slice 2.

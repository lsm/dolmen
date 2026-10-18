# Object storage

Tracking issue: #613. The contract decisions are settled; the last section records them.

## Problem

Agents keep rows that describe binary objects: the recording behind a transcript, the PDF behind an
extracted record, the image behind a caption. Today the only place for the bytes is a `json` or
`string` value, which caps them at the 32 MiB request body, pushes them through JSON as base64,
loads them whole into memory on every read, and puts them in the agent's context when an agent reads
the row. Agents rarely need the bytes themselves; they need to attach a file to a row, find it again,
and hand it to the tool that processes it.

## Agent UX

Dolmen's callers are agents, so the design is judged by two measures: how few tool calls a task
takes, and how little context the agent spends understanding the result. Agents run `curl`
fluently, so a plain HTTP request counts as one tool call like any MCP call.

The common case is an agent that fetched a file with one tool (a download, a generated export, a
recording) and holds it on disk without reading it. Attaching it to a row must then be **one call**
with no state carried between calls:

```bash
curl -F 'request={"namespace":"app","table":"meetings","records":[{"title":"Standup","recording":{"part":"rec"}}]}' \
     -F 'rec=@standup.ogg' http://127.0.0.1:8790/v1/insert
```

When the file is still at a URL, the agent does not even download it: the write names the URL and
the server fetches it, again in one call. Reading a row costs a small reference, never the bytes;
fetching the bytes is one `curl -o`. Every refusal says which call to make instead.

## Shape

- A **`file` field type**. The row holds a small reference; the bytes live in a blob store beside
  the namespace. Every existing read, search, `query`, filter and change-feed path keeps working on
  the reference.
- **Bytes ride on the ordinary write operations** as `multipart/form-data` on `/v1/{op}`, or are
  fetched by the server from a URL. There is no separate upload step and no upload id.
- **One download endpoint**, `GET /v1/files`, addressed by row.
- **Content addressed by SHA-256.** A blob is stored once per namespace however many rows reference
  it, a retried write stores nothing new, and the hash is the integrity check and the `ETag`.
- **A pluggable blob store** behind one small interface: a local directory first, an S3-compatible
  store later. Both are pure Go, so the build stays `CGO_ENABLED=0`.
- **Garbage collected by `vacuum`**, by mark and sweep over the file columns, never by reference
  counts.

Out of scope: transcoding, thumbnails, transcription or text extraction, encryption of blobs at rest,
and full-text or vector search over file contents. Each is a separate tool that reads the file
through the download endpoint and writes its result back as ordinary fields.

## The `file` type

**Declaring it.** `{"name": "recording", "type": "file"}`, with two optional constraints:

- `max_bytes`: the largest blob the field accepts. The server's `-max-file-size` still applies, so
  the lower of the two wins; a declaration is not checked against the flag, which can change
  between restarts.
- `media_types`: accepted media types, exact (`application/pdf`) or a top-level wildcard
  (`audio/*`).

`fulltext`, `vectorize`, `enum`, `shape` and `default` are refused on a file field, at
`create_table` and through every `migrate` op, as they are on `secret`. A file field cannot be an
`upsert_by_key` key. `required` is allowed. `migrate` gains `set_file_constraints` to change
`max_bytes` and `media_types`. Like `set_enum` it refuses when stored references violate the new
constraint, reporting how many rows do, and like `set_shape` it names their ids, withholding both
from a row-scoped caller as those ops do. On lakehouse the plan follows logical file fields
through rename/drop/add changes, validates each constraint in order against stored reference
sizes and media types, and changes only catalog metadata; it does not rewrite Parquet or blobs.
Because it reads rows, it joins `Change.ReadsRows` and needs
the `read` verb under `-auth on`.

**The value on read.** A non-null file value reads back as an object:

```json
{"sha256": "9f86d0…", "size": 48213, "media_type": "audio/ogg", "name": "standup.ogg"}
```

Every read path returns the same object: `read_rows`, both searches, `query` (by result-column label,
as for every declared type), and the facade. `size` comes from the blob. `media_type` and `name`
belong to the reference, not the blob: identical bytes attached as `audio/ogg` in one row and
`application/octet-stream` in another are one blob with two references, each keeping its own type.
`name` is `null` when nothing supplied one. It is validated wherever it enters: 1 to 255 bytes of
UTF-8, no control characters, no `/` or `\`; otherwise `invalid_request` naming the rule.

**The value on write.** A file value names where the bytes come from, never a hash:

| Value | Bytes come from | Where |
|---|---|---|
| `{"part": "rec"}` | the multipart part named `rec` in the same request | `/v1` only |
| `{"url": "https://…"}` | a server-side fetch of that URL | `/v1` and MCP; off unless `-files-fetch-hosts` is set |
| `{"copy_from": {"table": "meetings", "id": 42, "field": "recording"}}` | another row's file | `/v1` and MCP |
| `null` | clears the field | everywhere |

Each form also takes optional `name` and `media_type`, which override what the source supplied.
Writing back the object a read returned is refused with an error that says which of the forms to use.
Over MCP, a `part` value is refused with an error that gives the `curl -F` form of the same request.

After the bytes are known, every form is checked the same way against the field's current
`max_bytes` and `media_types`, so tightening them with `set_file_constraints` applies to the next
write immediately.

Why never a hash: content addressing turns a hash into a capability. If a write could attach a hash,
any caller who knows or guesses one, for example the hash of a well-known document or one seen in a
log, could attach a blob it never had and then download it through a row it can read. Each form above
proves access to the bytes: the caller sent them, the server fetched them, or the caller can already
read a row holding them.

**`copy_from`** (slice 2). The source must be a row the caller could read with `read_rows` in the
same namespace: the `read` verb, or own-row access on a `row_access` table. Any other row, a missing
one, or a null file value is the same `invalid_request`, so it does not confirm that a row exists.
The reference copies the source's `sha256`, `media_type` and `name`. It resolves inside the write
transaction under the namespace writer, where the source row's reference keeps the blob marked, so no
sweep can collect it in between.

**Storage in the row.** The column holds the whole reference as JSON (`sha256`, `size`,
`media_type` and `name`). A hash always names the same bytes, so storing its size in each
reference never goes stale, and a read needs no join. SQLite stores it as TEXT,
PostgreSQL as `text` like every `json` field there, the lakehouse as a Parquet string, so `query` and
filters can read the hash with each dialect's JSON accessor (`json_extract(recording, '$.sha256')`
on SQLite and DuckDB, `recording::jsonb->>'sha256'` on PostgreSQL).

## Writes with files

**Multipart.** `insert`, `update`, `upsert`, `upsert_by_key` and `batch` accept
`multipart/form-data` on `/v1/{op}` as well as JSON:

- The first part must be named `request` and holds exactly the JSON body the operation takes
  today. It is bounded by the existing 32 MiB request limit. A request whose first part is anything
  else is refused before any file is read, naming the rule.
- Every later part is a file, named by its form name. Its `filename` supplies the default `name`. Its
  `Content-Type` supplies the default `media_type`; when it is absent or `application/octet-stream`
  (what `curl -F x=@file` sends), the server infers the type from the filename's extension, then by
  sniffing the first 512 bytes. So `-F rec=@standup.ogg` arrives as `audio/ogg` with no `type=`.
- A `part` value may be referenced from any number of records in the request. A reference to a part
  that is absent is `invalid_request` naming the parts the request does carry, which is how a typo
  shows up. A part no value references is never stored: it is discarded with the request's other
  temporary files. At most 100 file parts per request, and at most `-max-request-files-size` of
  file bytes across them.
- `media_types` filters on the declared or inferred type. It is not a content check: a caller can
  declare `application/pdf` over any bytes. What keeps a mislabeled file harmless is the download's
  headers.

The server first authorizes the `request` part as it would a JSON body, so a caller without the
write's verbs streams nothing. It then streams each part to a temporary file under
`<data>/.files-tmp/` while hashing it, and refuses a part as soon as it passes `-max-file-size`, the
request's `-max-request-files-size` or the server's `-max-pending-files-size`. Streaming takes no
lock and knows no schema, so a field's `max_bytes` and `media_types` are checked when the write
resolves the value, once the namespace and table are known; the refusal names the field and its
limit.

Resolving a `{"part": …}` value places the part's file: if the namespace already stores that hash
the temporary file is discarded, otherwise it is renamed into `<data>/<ns>.files/` (healing a file
that had gone missing), counted against `-max-namespace-files-size`, and the directory synced. The
hash is then pinned until the request ends, which is what keeps the sweep (below) from collecting a
file whose write has not committed yet. If the write then fails, the request removes a file it
created that no other request has attached since; anything else waits for the next sweep; every temporary file is deleted when the request ends either
way.

A `batch` may carry parts for any of its writes; they share one `request` and one transaction.

**Idempotency.** The request fingerprint covers the `request` JSON with each file value
canonicalized whole: a `part` value becomes the part's SHA-256 plus the effective `media_type` and
`name` after any override in the value, and a `url` value becomes the URL string plus the value's
own `name` and `media_type` overrides, if any. A retry with the same idempotency key, the same bytes
and the same overrides replays; anything else under the same key is a conflict, as a different body
is today.

**Timeouts.** A multipart write outlasts the server's absolute `-read-timeout`, so, as `subscribe`
already does for its frames, the handler re-arms the read deadline through `http.ResponseController`
before each chunk, which makes it an idle timeout of that length. `-op-timeout` starts once the body
has arrived and bounds the write itself; with `-files-store s3` a multipart write also gets
`-files-fetch-timeout` added, since its parts still upload to the bucket inside the write; a write
that both fetches a `url` and uploads parts gets it added twice. The server arms its write deadline when it reads the request
headers, so the handler re-arms it before answering, as every other response path does.

**Server-side fetch** (slice 2). A `url` value makes the server fetch the bytes before taking the
writer, streaming them to a temporary file under the same limits. The engine fetches
when it resolves the value, through the request's file source, and each URL once per request. A
write whose body mentions `"url"` gets `-files-fetch-timeout` added to its `-op-timeout`, and the
fetch itself is bounded by `-files-fetch-timeout`. The engine looks an idempotency key up before it
resolves any value, so a retry whose key is recorded replays without fetching, even after its
source URL has expired or changed; the fingerprint covers the URL string and the value's overrides. It is off unless the operator
sets `-files-fetch-hosts`: a comma-separated list of host names, where `*.example.com` matches
subdomains and `*` alone matches any public host. Without it, a `url` value is refused with an error
that gives the `curl -o` then `curl -F` alternative. Every fetch:

- allows only `http` and `https`, at most 5 redirects, each re-checked against the list;
- resolves the host itself and refuses loopback, private, link-local, unique-local, multicast and
  unspecified addresses at connect time, so DNS rebinding cannot reach them;
- sends no dolmen credentials and no caller headers;
- takes `media_type` from the response's `Content-Type`, inferred as for parts when it is
  `application/octet-stream`, and `name` from `Content-Disposition` or the URL path's last segment;
- gives up after `-files-fetch-timeout`.

A fetch that fails is `invalid_request` naming the host and the HTTP status or network error class,
never the response body; a `5xx` or a timeout says the fetch may be retried. Under `-auth on`, a
`url` value needs no more than the write it is part of.

## Download

`GET /v1/files?namespace=<ns>&table=<t>&id=<row id>&field=<f>`, a plain handler beside
`/v1/subscribe` at an exact `/v1/files` pattern, which `ServeMux` prefers over the `/v1/` subtree.
It honors `-prefix`, authentication, and the request-id and tracing middleware. `files` becomes a
reserved operation name: an operation by that name would be shadowed over HTTP yet still callable
through MCP `tools/call`, breaking transport parity.

- Addressed by row, not by hash, so authorization is exactly `read_rows` on that row: the `read`
  verb, or own-row access on a `row_access` table. A row the caller cannot see answers `404`, as
  `read_rows` would leave it out.
- Answers with the bytes through `http.ServeContent`, so `Range`, `If-None-Match` (the `ETag` is the
  quoted hash) and `HEAD` work. The handler re-arms the write deadline before each chunk, so
  `-write-timeout` is an idle timeout here.
- Always sets `Content-Type` to the row's media type, `X-Content-Type-Options: nosniff`,
  `Content-Disposition: attachment` with the stored name encoded per RFC 6266 (an ASCII `filename`
  with quotes and non-ASCII replaced by `_`, plus `filename*` in RFC 5987 UTF-8 form), and
  `Content-Security-Policy: sandbox`. Dolmen serves its sign-in and token pages on the same origin,
  so an uploaded HTML or SVG file must never render as a page there, where it could script or
  redress those pages and the API.

Slice 3 adds `file_url` (`namespace`, `table`, `id`, `field`, optional `ttl_seconds`, default 900, at
most 3600), an operation on both transports that returns `{"url", "expires_at"}`: a short-lived
download URL the agent can hand to a tool that holds no credentials. It needs the same reach as the
download. On the S3 store the URL is the store's own presigned `GET`, with `response-content-type`
and `response-content-disposition` set so the bytes arrive with the same type and as an attachment.
It names the blob, not the row, so replacing the row's file does not revoke it; it works until it
expires, at most an hour, unless a sweep removes the blob first. It needs no public base URL;
`dolmen mcp` over stdio still refuses `file_url`, because that transport has no file store at all.
On the local store it is the download endpoint with `exp` and `sig` query parameters: an HMAC-SHA256
over the namespace, table, row id, field, hash and expiry, under a key generated on first use and kept
at `<data>/files.key` (mode `0600`). The signature binds the hash, so replacing the row's file
invalidates every URL issued for the old one.

A local URL is absolute, built from the same public base URL the served skills use
(`DOLMEN_BASE_URL`, else the request's host and forwarding headers, plus `-prefix`). Over the
stdio MCP transport there is no HTTP endpoint to link to, so `file_url` there is
`invalid_request` saying to call it over HTTP. A signed download needs no credential: the
signature is the authorization. The link carries the hash it was issued for as `sha256`, so the
signature is checked over the link's own parameters before any row is read; only then is the row
read and its current hash compared with the link's.
A link that is expired answers `403` `forbidden` naming its expiry; one whose signature does not
match, whose row is gone, or whose file was replaced answers one `403` `forbidden` for all three,
so a forged link learns nothing about which rows exist. Both say to call `file_url` again.

## Errors

| Case | Status | Code |
|---|---|---|
| Missing namespace, table, field or row on download, or a null file value | `404` | `not_found` |
| Field is not a `file` field | `400` | `invalid_request`, naming the field's type |
| A file over the field's `max_bytes` or `-max-file-size` | `413` | `invalid_request`, naming the part or URL and the limit |
| Media type not in `media_types` | `415` | `invalid_request`, naming the accepted types and saying to set `media_type` if the guess was wrong |
| `request` part missing or not first; a reference to a missing part | `400` | `invalid_request`, naming the rule or the part |
| `part` over MCP; `url` with fetching off | `400` | `invalid_request`, giving the `curl` form to use instead |
| Fetch failed | `400` | `invalid_request`, naming the host and the status or error class |
| Disk full | `507` | `internal_error`, as `-max-namespace-size` refusals today |
| Over `-max-namespace-files-size` | `507` | `internal_error`, naming the flag, the namespace's current file bytes and the cap |
| Request over `-max-request-files-size` | `413` | `invalid_request`, naming the flag |
| Server over `-max-pending-files-size`, or the blob store unreachable (S3, slice 3) | `503` | `storage_unavailable`, a new code following `sql_engine_unavailable`; retryable |

## Skills

The served skill teaches the three recipes in a few lines, because that text is what an agent reads
before its first call: attach a file on disk with `curl -F 'request=…' -F 'name=@path' /v1/<op>`,
attach a URL with `{"url": …}` when the server allows it, and fetch bytes with
`curl -o path '/v1/files?…'`. It says a read returns a reference, not the bytes.

## Go facade

Record values may be a `dolmen.FileUpload{Reader io.Reader, Name, MediaType string}` (`dolmen.File` is already the field type constant), which the facade
streams exactly as a multipart part, or a `dolmen.FileURL` or `dolmen.FileCopy`. `OpenFile(ctx,
namespace, table string, id int64, field string) (io.ReadSeekCloser, FileInfo, error)` reads one.
The facade has no auth. Server-side fetch needs `WithFileFetch(hosts []string, timeout
time.Duration)`, mirroring `-files-fetch-hosts` and `-files-fetch-timeout`; without it a
`dolmen.FileURL` value is refused, as on a server without the flag, since `Open` reads no flags or
environment. Every other rule is the same.

## Blob store

```go
type BlobStore interface {
	Stat(ctx context.Context, ns, sha256 string) (BlobInfo, error)
	Put(ctx context.Context, ns, sha256, path string) error
	Open(ctx context.Context, ns, sha256 string) (Blob, error)
	Delete(ctx context.Context, ns, sha256 string) error
	List(ctx context.Context, ns string, fn func(sha256 string, info BlobInfo) error) error
	DropNamespace(ctx context.Context, ns string) error
}
```

Parts and fetches are hashed while they stream to `.files-tmp/`, so a store never hashes: `Put`
takes a finished temporary file already named by its hash, and the file is gone once it returns.
`BlobInfo` is the blob's size and modification time; `Blob` is an `io.ReadSeekCloser` with its
size. Pins, attach counts and the namespace totals stay in `files.Store` above the interface, so
every store shares them. The engine never sees bytes. A new `internal/files` package owns the interface, the local
implementation, multipart and fetch streaming, the download handler and the sweep; each engine owns
only the file column and the reference scan the sweep needs.

**Local.** `<data>/<ns>.files/<first two hex>/<sha256>`, written to `<data>/.files-tmp/` and
renamed. Neither name is a valid namespace segment, so `list_namespaces` never sees them.
SQLite and the lakehouse put it beside the namespace's `.db` or `.lakehouse/` directory.

**S3** (slice 3). `-files-store s3` keeps blobs in an S3-compatible bucket through `minio-go`, which
is pure Go. A blob is the object `<prefix><ns>.files/<first two hex>/<sha256>`, mirroring the
local layout, so dropping `a` never touches `a/b`'s blobs. Parts and fetches still stream to
`<data>/.files-tmp/` under the same limits, then upload once their hash is known; an object that
already exists is not uploaded again. S3 caps a single `PUT` at 5 GiB, so a larger blob uploads
as a multipart upload; an S3 object is capped at 5 TiB, so with `-files-store s3` a
`-max-file-size` above 5 TiB refuses to start, naming the ceiling. The store is configured by flags, each with a
`DOLMEN_FILES_S3_*` environment variable:

| Flag | Default | Meaning |
|---|---|---|
| `-files-s3-bucket` | none, required | Bucket name |
| `-files-s3-endpoint` | `https://s3.amazonaws.com` | Endpoint URL; `http://` is allowed, for a local MinIO |
| `-files-s3-region` | empty | Region; empty lets the client discover it |
| `-files-s3-prefix` | empty | Key prefix, so one bucket can serve several deployments |
| `-files-s3-path-style` | `false` | Path-style addressing, which MinIO and some other S3-compatible stores need |

Credentials never come from flags. The client's standard chain reads `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN`, then `~/.aws/credentials`, then an IAM or IRSA
role. Startup checks that the bucket is reachable and refuses to start otherwise, naming the
bucket and the error class. Once running, a store error is `503` `storage_unavailable`.

**PostgreSQL.** Rows live in the database but the local store is on one server's disk, so two servers
on one catalog would each see half the blobs, and nothing in the engine can tell that a second
server exists. So until the S3 store exists (slice 3), the postgres engine refuses a `file` field at
`create_table` and `add_field` with `invalid_request` saying file fields need `-files-store s3` on
PostgreSQL. From slice 3 it accepts them only with the S3 store configured.

**Metadata.** There is no blob table. The store's directory is the set of stored hashes, and each
reference carries its size. A blob table would have to be written in the same transaction as every
write on every engine; the pins below (and on S3, PostgreSQL's namespace lock) give the same
safety with no schema change.

## Garbage collection

`vacuum` gains a sweep (slice 1b), run holding the namespace's writer as `vacuum` already does, so no
write commits during it. It marks every hash referenced by a file column in any table of the
namespace, then deletes each stored file that is unmarked and not pinned, subtracting its size from
the namespace's total so files left by failed writes stop counting toward
`-max-namespace-files-size`. A failed write deletes a file it placed itself only when no other
request has attached that hash since, because a reference can only come from an attach; otherwise
the file waits for the sweep. The pin check and the
unlink happen under the same lock that placement takes, so a request either pins its hash before the
sweep looks at it, and the file is kept, or places it after, and finds it gone and stores it again.
A write that committed before the sweep is marked by its reference; one still in flight holds a pin.
Neither order leaves a row pointing at a deleted file.

On lakehouse, the namespace gate and exclusive maintenance barrier cover compaction,
checkpointing, a streaming scan of the current snapshots' file columns, and the sweep. A failed
maintenance or reference scan does not sweep any blobs. File bytes stay outside the namespace's
row/catalog size accounting.

The same pass deletes temporary files under `.files-tmp/` that no request in this process holds
open; after a restart every one is an orphan. Pins live in memory, which is sound because one data
directory has one live server.

**On S3 the pins are not enough.** Several servers can share one PostgreSQL catalog and one bucket,
and each holds only its own pins, so one server's sweep could delete a blob another server has just
attached but not yet committed. PostgreSQL already serializes a namespace's writes across servers:
every write takes the namespace's catalog row `FOR NO KEY UPDATE` in its transaction. The S3
design orders blobs by that lock instead of by pins:

- A part or fetched file uploads to its final key before the write takes the lock, so the lock is
  not held for an upload, except inside a `batch`: its writes share one transaction and a later
  write's `copy_from` may name a row an earlier one created, so a batch resolves its file values
  inside that transaction and holds the namespace lock while its parts upload. Send large files
  in single writes rather than in a batch.
- Inside the write's transaction, after its rows are written and before it commits, every blob the
  write attached (a `part`, a `url` or a `copy_from`) is checked to still exist. If a sweep removed
  one in between, the write rolls back with `503` `storage_unavailable` saying to retry it; the
  retry uploads the file again. The race needs a `vacuum` to land in the gap, so a retry is
  cheaper than keeping every temporary file for a re-upload.
- The sweep marks and deletes in one transaction holding the same row lock, so no write can attach
  between its scan of the file columns and its last delete.
- A failed write on S3 never deletes a blob it uploaded, since another server may have attached it
  after the lock was released; the next sweep collects it.

So a committed reference always names an object that existed while its write held the lock, and
no sweep can run between that check and the commit. The local store keeps the pin rule, whose
check and unlink are atomic under one process's lock.

Reference counts were rejected: every write path (insert, update, upsert, `upsert_by_key`, `batch`
with its rollback, `drop_field`, `drop_table`, the lakehouse's replayed commit log) would have to keep
them exact, and one missed decrement leaks a blob forever while one extra deletes a blob a row still
references. A full scan is only paid at `vacuum`, which already rewrites the namespace.

`drop_table` and `drop_field` leave their blobs for the next sweep. `drop_namespace` removes the
whole blob directory. `vacuum`'s result gains `files_removed`, `file_bytes_removed` and `file_bytes`, the namespace's
stored file bytes after the sweep. No other operation's output changes shape.

## Limits and accounting

| Flag | Default | Meaning |
|---|---|---|
| `-max-file-size` | `1GiB` | Largest single file, whatever a field's `max_bytes` says |
| `-max-namespace-files-size` | `0` | Largest total of one namespace's stored blobs, as bytes or with `KiB`/`MiB`/`GiB`/`TiB`; `0` is unbounded |
| `-files-store` | `local` | Blob store: `local`, or `s3` (configured as in "Blob store") |
| `-files-fetch-hosts` | empty | Hosts a `url` value may fetch from; empty disables fetching |
| `-files-fetch-timeout` | `5m` | Longest one server-side fetch may take |
| `-max-request-files-size` | `2GiB` | Most file bytes one request may stream, across all its parts and `url` fetches |
| `-max-pending-files-size` | `8GiB` | Most file bytes all in-flight requests may hold in `.files-tmp/` at once, server-wide |

The 32 MiB request limit bounds the `request` part only; file parts answer to `-max-file-size`.
Blobs do not count toward `-max-namespace-size`, which keeps bounding the namespace's rows;
`-max-namespace-files-size` bounds its blobs separately, so a large file can never refuse the
namespace's row writes. The total is the size of every file the namespace stores, each counted once
however many rows reference it, kept in memory after one walk of the directory. It is checked when a
part is placed, under the store's lock, once its hash is known: bytes already stored add nothing and
are always accepted, and concurrent writes cannot overshoot the cap together. A file placed by a
write that then failed counts until the next sweep removes it.

Temporary space is bounded before placement by the last two flags: a request stops as soon
as its own streamed bytes pass `-max-request-files-size`, refused as `413` `invalid_request` naming
the flag, and a chunk that would push the server's in-flight total past `-max-pending-files-size` is
refused as `503` `storage_unavailable`, saying the server is busy with other uploads and the write
may be retried. So `.files-tmp/` never holds more than `-max-pending-files-size`, whatever the number of
parts or concurrent requests, and a refused write frees its share at once.

The namespace total is kept in one server's memory, which several servers sharing a bucket cannot
keep exact, so a non-zero `-max-namespace-files-size` with `-files-store s3` refuses to start,
saying the cap needs the local store. The temporary-space flags stay per server and apply to S3. The refusal of a write
over the namespace cap names the namespace's current file bytes, and `vacuum` reports them.

## Change feed, backup, auth

Change records carry ids, never values, so attaching or replacing a file is an ordinary `update`
change. `dolmen backup` copies every blob a namespace snapshot references, read from the snapshot
itself so the copy matches the rows, and lists each blob's path, size and hash in its manifest
(format 2 when any are present, so an older binary refuses rather than restoring rows without
their files). `restore` verifies every blob before writing anything and places blobs before the
namespaces that reference them. A referenced blob missing from the data directory fails the
backup, naming the namespace and hash. No new verb: attaching a file is part of a write and downloading is a
read. CORS needs no change: multipart writes are `POST` and downloads are `GET`.

## Conformance

Pinned on every engine that accepts `file` fields: the declaration refusals, the read-back object,
each write form and its refusals (a raw hash, a read-back object, a reference to a missing part, a
part over MCP, an unreadable `copy_from` source, a `url` with fetching off), media type inference
from the filename, multipart and `url` idempotent replay and conflict (a `url` replay succeeding
after the source stops answering), a failed write storing nothing,
download with `Range` and `ETag`, the download's safety headers, row-scoped download authorization,
the files cap, and that `vacuum` removes exactly the unreferenced blobs. MCP parity is that both
transports read the same reference and accept the same `url` and `copy_from` values. The fetch guard
is pinned against a local stub: loopback and private targets, a redirect into them, and a host outside
the list.

## Slices

Each one a PR from main.

1. On SQLite, in two PRs:
   - 1a: the `file` type, multipart writes, the download endpoint, the local store with its pins,
     the files cap and temporary-space bounds with `storage_unavailable`, skills, README and
     conformance; PostgreSQL and the lakehouse refuse `file` fields, and `dolmen backup` refuses a
     data directory that stores files, naming the namespace, since it cannot copy them yet.
   - 1b: the sweep in `vacuum` and `drop_namespace` removing the store, `set_file_constraints`,
     and the facade's `dolmen.File` and `OpenFile`.
2. Lakehouse parity, `copy_from`, and server-side fetch. Lakehouse parity is implemented in five PRs:
   shared coercion/copy helpers and request fingerprints; dormant lakehouse write coercion;
   vacuum reference scans and sweeping; stored-reference checks for `set_file_constraints`;
   then declaration enablement, transport conformance and documentation. The shared copy helper
   reads only table metadata and rows; lakehouse marks the namespace gate as already held while
   resolving a file, so both the copy reader and its authorization callback can read metadata
   without reacquiring the gate.
   Write pre-checks parse file values without fetching, attaching or pinning; coercion resolves
   them once after matching rows. Declarations stayed disabled until sweeping and constraint
   checks landed. The fingerprint helpers take the request's context and canonicalize file values
   for every engine, retaining SQLite's existing hashes. SQLite and lakehouse now accept file
   declarations; PostgreSQL accepts them with `-files-store s3` (slice 3).
3. The S3-compatible store, `file_url`, and `file` fields on PostgreSQL over
   the S3 store, in PRs of their own: the `BlobStore` interface with the local store behind it;
   `file_url` with signed local URLs; the S3 store and its flags; the S3 sweep under PostgreSQL's
   namespace lock; then PostgreSQL accepting `file` fields with the S3 store, with transport
   conformance and documentation.
4. `dolmen backup` and `restore` with blobs.

## Decisions

Settled on 2026-10-14.

1. **`storage_unavailable` (`503`)** for an unreachable blob store or a server out of temporary
   upload space, so an agent can tell a transient condition it may retry from a server fault, and operators can alert on it separately. It is wired
   through `derr`, the API envelope, the OpenAPI error enum, a `dolmen.ErrStorageUnavailable` facade
   sentinel, the skills and the README, as `sql_engine_unavailable` was.
2. **Attaching a file is one call.** Bytes ride on the write as multipart, or the server fetches a
   URL; there is no separate upload step, upload id or upload tool. `file_url` hands out download
   links. Agents use `curl` as readily as MCP, so a multipart request is as available to them as a
   tool call, and it never puts the bytes in their context.
3. **PostgreSQL refuses `file` fields until the S3 store exists.** A local store there would split the
   blobs between servers silently.
4. **A separate `-max-namespace-files-size`.** Blobs stay out of `-max-namespace-size`, so a large
   file cannot block row writes.
5. **`copy_from` is allowed**, in slice 2.

Settled on 2026-10-17.

6. **The S3 sweep is ordered by PostgreSQL's namespace lock**, because servers sharing a bucket cannot
   see each other's pins: an attach checks its object under the lock writes already take, and the
   sweep marks and deletes under the same lock. A grace window on object timestamps was considered
   first and rejected: a sweep could list a blob, another server refresh it, and the sweep still
   delete it, since S3 has no portable conditional delete, and refreshing needed a self-copy. Pins
   in PostgreSQL were rejected because every file write would pay extra round trips and a crashed
   server's pins would need expiry. Leaving S3 to bucket lifecycle rules was rejected because blobs
   would leak by default. The per-namespace files cap is refused with S3 rather than enforced
   inexactly.
7. **`minio-go` is the S3 client**: one pure-Go module that works with any S3-compatible service,
   with presigned `GET` and a standard credential chain built in. `aws-sdk-go-v2` was heavier, and
   hand-rolled SigV4 would make us own the signing code.
8. **S3 is configured by flags, with credentials from the standard chain only**, never from a flag,
   so secrets stay out of `ps` and IAM roles work on Kubernetes.

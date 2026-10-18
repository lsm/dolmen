# Running file storage

A `file` field keeps a reference in the row and the bytes outside the tables, in a file store. This
guide is for the operator: choosing and configuring the store, sizing its limits, keeping it tidy,
and backing it up. How agents write and read files is in the served skills (`/skills/dolmen`,
"Files") and the README.

## Which store

| Engine | `-files-store local` (default) | `-files-store s3` |
|---|---|---|
| `sqlite` | Yes | Yes |
| `lakehouse` | Yes | Yes |
| `postgres` | Refused: one server's disk would hide the files from the others sharing the catalog | Yes |

Pick `local` for one server on one disk; `dolmen backup` then copies the files with the namespaces.
Pick `s3` when several servers share a PostgreSQL catalog, when files outgrow the disk, or when you
already run object storage. Both stores behave the same to callers.

Choose before the first file is written. There is no tool that moves files between the stores, and
a server started on the other store does not find the files its rows reference.

Files are content-addressed by SHA-256 within a namespace: identical bytes written by any number
of rows are stored once. `copy_from` reuses a stored file without an upload, and only within the
same namespace.

## The local store

Each namespace's files live beside it in the data directory:

```
<data>/<namespace>.files/<first two hex digits>/<sha256>
<data>/.files-tmp/          uploads in flight, removed when each request ends
<data>/files.key            the key that signs file_url links, created on first use
```

They inherit the data directory's permissions. Keep them on the same local filesystem as the data
directory. To invalidate every `file_url` link already issued, stop the server, delete `files.key` and
start it again; it makes a new key on the next call. A running server keeps the key it loaded.

## The S3 store

```bash
AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… \
dolmen -files-store s3 -files-s3-bucket dolmen-files -files-s3-region us-east-1
```

| Flag | Environment variable | Meaning |
|---|---|---|
| `-files-s3-bucket` | `DOLMEN_FILES_S3_BUCKET` | The bucket; required. It must exist: startup refuses a missing or unreachable bucket |
| `-files-s3-endpoint` | `DOLMEN_FILES_S3_ENDPOINT` | Endpoint URL, `https://s3.amazonaws.com` by default; `http://` is allowed for a local MinIO |
| `-files-s3-region` | `DOLMEN_FILES_S3_REGION` | Region; empty lets the client discover it |
| `-files-s3-public-endpoint` | `DOLMEN_FILES_S3_PUBLIC_ENDPOINT` | Endpoint `file_url` links are presigned for, when clients reach the bucket at another address than the server does; uploads and reads keep using `-files-s3-endpoint` |
| `-files-s3-prefix` | `DOLMEN_FILES_S3_PREFIX` | Key prefix, so several deployments can share one bucket |
| `-files-s3-path-style` | `DOLMEN_FILES_S3_PATH_STYLE` | Path-style addressing, which MinIO and some S3-compatible services need |

Objects are keyed `<prefix><namespace>.files/<first two hex digits>/<sha256>`.

Credentials never come from a flag. The standard chain is used: `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN`, then `~/.aws/credentials`, then an IAM or IRSA role.
They need these permissions on the bucket and its keys:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["s3:ListBucket", "s3:GetBucketLocation"], "Resource": "arn:aws:s3:::dolmen-files"},
    {"Effect": "Allow",
     "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"],
     "Resource": "arn:aws:s3:::dolmen-files/*"}
  ]
}
```

`s3:GetBucketLocation` is what lets the client discover the region when `-files-s3-region` is empty. With a prefix, narrow the object resource to `arn:aws:s3:::dolmen-files/<prefix>*`.

A MinIO or another S3-compatible service:

```bash
AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=… \
dolmen -files-store s3 -files-s3-bucket dolmen-files \
  -files-s3-endpoint http://minio:9000 -files-s3-region us-east-1 -files-s3-path-style
```

`file_url` on this store returns the bucket's own presigned `GET`, built on `-files-s3-endpoint`
unless `-files-s3-public-endpoint` is set. When the server reaches the bucket at an address clients
cannot (a loopback MinIO, a private VPC endpoint), set `-files-s3-public-endpoint` to the address
they use, such as `https://files.example.com`; otherwise the links open for nobody but the server.

The first start with `s3` writes `<data>/files.store`, which makes `dolmen backup` and
`dolmen restore` refuse that data directory, since the files are not in it.

Several servers may share one bucket, as they do with PostgreSQL. A `vacuum` on one server can
remove a file another server's write has just uploaded but not yet committed; that write checks its
files once it holds the namespace's write lock, finds one gone, and fails with `503`
`storage_unavailable` without writing anything, so retrying it uploads the file again.

## Limits

| Flag | Default | What it bounds | Past it |
|---|---|---|---|
| `-max-file-size` | `1GiB` | One file; a field's `max_bytes` can only lower it. At most `5TiB` with `s3` | `413` `invalid_request` |
| `-max-request-files-size` | `2GiB` | All file parts of one multipart write; at most 100 parts | `413` (`400` past 100 parts) |
| `-max-namespace-files-size` | `0` (unbounded) | One namespace's stored files, each counted once. Local store only | `507`, nothing written |
| `-max-pending-files-size` | `8GiB` | Bytes all in-flight writes hold in temporary space, server-wide | `503` `storage_unavailable`, retry |

A field's `media_types` refuses other types with `415`. The `request` part of a multipart write
counts toward the ordinary 32 MiB body limit.

## Fetching from URLs

A `{"url": "https://…"}` value has the server download the file. It is off until hosts are listed:

```bash
dolmen -files-fetch-hosts 'raw.githubusercontent.com,*.example.com' -files-fetch-timeout 5m
```

Entries are exact host names, `*.example.com` for subdomains, or `*` for any public host. Whatever
the list says, a fetch is refused when the host resolves to a loopback, private, link-local or
otherwise non-public address, checked again on every redirect, and no credentials or cookies are
sent. Fetches identify as `User-Agent: dolmen (+https://github.com/lsm/dolmen)`.
`-files-fetch-timeout` (`1s` to `24h`) bounds one fetch and is added to the write's `-op-timeout`.

## Unreferenced files and `vacuum`

Deleting a row, clearing its file field or replacing the file leaves the old bytes stored. They
still count toward `-max-namespace-files-size`, and `vacuum` removes them:

```bash
curl -s localhost:8790/v1/vacuum -H 'Content-Type: application/json' -d '{"namespace":"app"}'
```

It reports `files_removed`, `file_bytes_removed` and `file_bytes`, what the namespace still stores,
on every engine. A vacuum that fails before its sweep removes nothing; one whose sweep fails part-way keeps what it already removed, which is only ever files no row referenced. Run it on a schedule for namespaces that replace
or delete files often, and before raising a quota that a namespace has hit. `drop_namespace` takes
its files with it, objects included on `s3`; `drop_table` leaves them for the next `vacuum`, since
another table of the namespace may reference the same bytes.

## Downloads

`GET /v1/files?namespace=&table=&id=&field=` needs the same access as `read_rows` on the row, and
serves the stored type as an attachment, with `nosniff`, a restrictive CSP, `ETag` and `Range`.

`file_url` hands out a link for a tool with no credentials, valid for `ttl_seconds` (900 by
default, at most 3,600). It is a bearer link: revoking the caller's key or grant does not end one
already issued. On the local store it is signed with `files.key` and also stops working once the
row's file is replaced or the row, table or namespace is gone. On `s3` it is the bucket's presigned
link and works until it expires. Links name the server by `-base-url` when it is set, otherwise by
the request's `Host`; set `-base-url` behind a proxy.

## Backups

- **Local store**: `dolmen backup` copies every file the snapshot references and checks each against
  its SHA-256; `dolmen restore` puts files back before the namespaces that reference them. See the
  README's "Backup and restore".
- **S3 store**: back up the bucket with S3 tooling, such as versioning or replication, and the
  namespaces separately: `pg_dump` on PostgreSQL; on SQLite or lakehouse, which `dolmen backup`
  refuses once the data directory is marked `files.store`, stop the server and copy the data
  directory, or take a filesystem snapshot. Files are deleted only by `vacuum` and
  `drop_namespace`, so a consistent pair is: back up the namespaces, then copy the bucket, with no
  `vacuum` and no `drop_namespace` in between. A bucket copy taken after the namespaces then holds
  every file they reference, plus possibly some no row needs, which the next `vacuum` removes.

## Errors to expect

| Status | Code | Meaning |
|---|---|---|
| `400` | `invalid_request` | A malformed file value, a missing part, a refused fetch, more than 100 parts |
| `413` | `invalid_request` | Past `-max-file-size`, a field's `max_bytes`, or `-max-request-files-size` |
| `415` | `invalid_request` | A type the field's `media_types` does not accept |
| `503` | `storage_unavailable` | The S3 bucket did not answer, a sweep removed a file mid-write, or `-max-pending-files-size` is full; retry |
| `507` | `internal_error` | `-max-namespace-files-size` reached (or the disk is full); delete or clear files, `vacuum`, or raise the limit |

# Secret fields

Tracking issue: #467. This records the decisions taken there and how slice 1 builds them.

## Decisions

1. **A `reveal` verb** (slice 2), which `admin` does not imply. `read` returns the mask; plaintext
   needs `reveal` on the table, its namespace, or `*`. `identity-and-engines.md` §2 carries the
   pending verb.
2. **Key rotation through `rotate_secret_key`** (slice 4). New writes use the new key at once; old
   values are re-encrypted in the background, batch by batch, with progress reported. Old keys stay
   available for decryption until nothing uses them.
3. **`WithSecretKey(key []byte)`** on the Go facade. Without a key, secret fields are refused, as
   vector features are without `WithEmbedding`.
4. **A fixed mask `"••••"`**, never a length or any other hint of the content.
5. **Every reveal writes an audit log line** (slice 2) with the principal, table, row id and field,
   never the value.

Slices, each one a PR from main: (1) this doc and the `secret` type on SQLite, (2) the `reveal`
verb under `-auth on` plus the audit log, (3) PostgreSQL parity, (4) `rotate_secret_key`.

## Slice 1

**Type.** `secret` is a new field type holding a string. `fulltext`, `vectorize`, `enum` and
`default` are refused on it, at `create_table` and through every `migrate` op, because each would
store or disclose the plaintext: an FTS index, an embedding, a list of allowed values, or a default
kept in the schema. A secret cannot be an `upsert` natural key, since each value is sealed under a
fresh nonce and equal plaintexts never compare equal. `required` is allowed.

**Key.** The server reads `DOLMEN_SECRET_KEY` (base64, 32 bytes) or `DOLMEN_SECRET_KEY_FILE`,
environment only, never a flag, and never logs it. It builds a keyring once and passes it to the
store with `store.WithSecretKey`. Without a key, a secret field cannot be created or added, and a
write carrying a non-null secret value is refused with an error naming both variables. Masked reads
need no key.

**Encryption.** AES-256-GCM (`internal/secret`). A stored value is a BLOB:

    version (1 byte, 0x01) | key id (8 bytes) | nonce (12 bytes, random) | ciphertext + tag

The key id is the first 8 bytes of SHA-256 of the key. It lets a wrong key fail with a teaching
error ("encrypted under a different key") instead of a bare authentication failure, and it is what
rotation will select old keys by. The version byte and key id are the GCM additional data. The
column name and row id are not: the id does not exist yet when an insert seals its values, and a
`rename_field` would orphan every value bound to the old name.

**Reads.** Every read path decodes a secret column to the mask when non-null and to null when null:
`read_rows`, both searches, `query` (by result-column label, as for every declared type), and the
facade equivalents. `read_rows`, `search_fulltext` and `search_vector` take `reveal`, a list of
secret field names to return in plaintext. Naming a field that is not a secret, or not in the table,
is `invalid_request`. A wrong key, a missing key, or a tampered value fails the reveal with
`invalid_request` and a message naming the cause. Reveal travels to the engine on the context
(`store.WithReveal`) rather than as a new `Engine` parameter; slice 2 revisits that when the verb
check needs the principal. With `-auth off` reveal is allowed. With `-auth on` the API refuses it
with `forbidden` until the verb exists; the facade has no auth and always allows it.

**Raw SQL.** `query`, search filters and write filters see only the ciphertext blob. Aliasing the
column (`SELECT token AS t`) returns base64 of the blob. Comparing a secret column with a plaintext
never matches.

**Change feed and backups.** Change records carry ids, never row values, so `changes_since`,
`wait_for` and SSE never hold a secret. Backups copy the database file, which holds only ciphertext.

**PostgreSQL.** `create_table` or `migrate add_field` with a secret field, and any `reveal`, is
refused on the postgres engine with a "not yet supported on the postgres engine" error until slice 3.

# The DuckDB sidecar (spike)

A minimal sidecar: it registers one namespace's Iceberg table at a pinned snapshot and runs caller
SQL, reading the protocol in [../driver/PROTOCOL.md](../driver/PROTOCOL.md). It exists to be measured
and attacked, not to be the final shape — see that document for what the protocol does not decide.

Confinement is applied through `DBConfig`, not a rc file, and the reason is worth knowing before
reading further: **`.duckdbrc` is read by the `duckdb` shell and not by an embedded library**, so a
sidecar that writes one looks configured and is not.

## Building: no DuckDB source build

The CI job downloads the prebuilt `libduckdb` for the target and the prebuilt `iceberg` and `httpfs`
extension files, and the sidecar links against them:

```
libduckdb-<platform>.zip    -> duckdb.hpp, libduckdb.so / .dylib / duckdb.lib
extensions.duckdb.org/<version>/<platform>/iceberg.duckdb_extension.gz
extensions.duckdb.org/<version>/<platform>/httpfs.duckdb_extension.gz
```

**The extension files are placed in the extension directory before the process starts and loaded by
path, so nothing downloads at runtime.** Both are published for every platform dolmen releases for,
which is why this spike needs no C++ toolchain beyond a compiler: the sidecar is ~270 lines of C++
against a prebuilt library.

One assumption this replaces, recorded in the plan as wrong:

- **No `make GEN=ninja EXTENSIONS='iceberg;httpfs'`.** A prebuilt library plus prebuilt extensions is
  enough, so there is no 40-minute DuckDB build per platform and no `ccache` to carry. Measured:
  **5 seconds and 64,632 bytes** for the shared-library build on `linux-amd64`.

**A fully static build is not available, though the artifacts look like they should provide one.**
The `-musl` zips ship `libduckdb_static.a` (83,882,636 bytes at v1.5.6) for both `linux_amd64` and
`linux_arm64`, and linking it fails twice over:

- the archive does not contain `duckdb::ExtensionHelper::LoadAllExtensions(duckdb::DuckDB&)`, so it
  is incomplete for embedding;
- it wants a **musl** toolchain — linking it with glibc's `g++ -static` fails on `res_init` and warns
  that `getaddrinfo` "requires at runtime the shared libraries from the glibc version used for
  linking".

So the sidecar ships as a binary, `libduckdb.so` (72 MB), and the two extension files, and it is not
static on any platform. `sidecar-packaging` records the failure rather than hiding it, so a DuckDB
that fixes the archive shows up as a changed line.

A source build remains the fallback if a platform ever lacks a prebuilt extension, and that fallback
is a toolchain-plus-long-build cost the release pipeline would then carry.

## Confinement: `DBConfig`, not `.duckdbrc`

**`.duckdbrc` is a CLI feature.** The `duckdb` shell reads it; an embedded `DuckDB(nullptr, &config)`
never looks at it. The first CI run of this spike wrote the settings to `$HOME/.duckdbrc` and **all 24
escape attempts succeeded** — `read_csv`, `glob`, URL tables, `ATTACH`, `COPY ... TO`, `INSTALL`,
`LOAD`, `getenv`, `CREATE EXTERNAL TABLE`, `SET`, DDL and DML alike. Nothing warns. The process looks
configured and is not, which is the worst failure mode a confinement mechanism has.

The settings go through `DBConfig`, split across two mechanisms because the ordering is load-bearing:

1. **`cfg.options.allowed_directories`** — a struct field, not a `SET`, because
   `allowed_directories` cannot be set once `enable_external_access` is false (#533 measured that).
2. **`cfg.options.unrecognized_options`** — `extension_directory`, `enable_external_access = false`,
   `autoinstall_known_extensions = false`, `autoload_known_extensions = false`,
   `allow_persistent_secrets = false`. DuckDB applies these at startup, before the database is
   usable. `enable_external_access` is the guard and has no struct field, so it goes here.
3. **`SET lock_configuration = true`** — last, by `SET` on a live connection, so nothing above can be
   reopened. The sidecar **exits non-zero if the lock is refused** rather than running unlocked.

`memory_limit` is a `DBConfig` struct field too (`cfg.options.maximum_memory`), for the same reason:
once `lock_configuration` is on, a `SET` cannot reach it.

**And `DBConfigOptions::access_mode = READ_ONLY`, which is not a substitute for any of the above.**
With only the external-access guard the CI battery found `CREATE TABLE`, `INSERT`, `UPDATE` and
`DELETE` all still accepted: `enable_external_access` is about *files*, and an in-memory catalog has
nothing external to guard. A query sidecar that must not change anything needs both.

`SIDECAR_UNLOCKED=1` starts the sidecar with none of this applied. That is what lets the driver tell a
refusal that is *our mechanism* from one that is merely an engine default: the same statement runs
against both starts and the outcomes are compared.

## Pinning a snapshot: `snapshot_from_id`, not `version`

`iceberg_scan`'s `version` parameter takes a **tag**, not a snapshot id. Pass it a numeric id and it
is **silently ignored** — the scan reads the current snapshot and returns plausible, wrong answers.
The correct parameter is `snapshot_from_id => <id>`, verified against a three-snapshot fixture
(50 / 51 / 51 rows across the three ids, matching the unpinned read).

This is the mirror image of the escape holes the confinement battery hunts: it does not let a reader
out of its namespace, it lets a *time-travel* reader quietly stop travelling, and it does so without
an error.

## Lifecycle

`cancel` calls `duckdb_interrupt` through the connection, which is cooperative and asynchronous: it
signals the running query and returns. A query that cannot be cancelled in time is handled by the
driver killing the process, which is the fallback the plan names.

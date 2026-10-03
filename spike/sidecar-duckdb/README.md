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

The lock is applied in `Seal()` (`sidecar.cpp`) when the first query arrives, before any caller
SQL runs, in this order:

1. **Write a private catalog file** read-write, holding one view per table, and close it. A
   read-only database cannot have views created in it.
2. **Reopen it with `DBConfig` struct fields**: `access_mode = READ_ONLY`, so DDL and DML are
   refused; `allowed_directories = {namespace dir}`, which has to be set before external access is
   closed; and `maximum_memory`, which a `SET` could not reach once the configuration is locked.
3. **`LOAD iceberg`** while loading is still allowed.
4. **`SET` on the open connection**, in order: `enable_external_access = false`,
   `autoinstall_known_extensions = false`, `autoload_known_extensions = false`,
   `allow_persistent_secrets = false`, then `lock_configuration = true` last. The sidecar **exits
   non-zero if any of these is refused** rather than running unlocked.

`DBConfigOptions::unrecognized_options` is **not** a way to set these: an earlier run put them there
and the process aborted with `The following options were not recognized: ...`.

`enable_external_access = false` and `access_mode = READ_ONLY` are not substitutes. The first is
about *files*; with only it, the battery found `CREATE TABLE`, `INSERT`, `UPDATE`, `DELETE` and
`DROP VIEW` all accepted.

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

Queries run on a worker thread against one connection, and `cancel` calls `Connection::Interrupt` on
it from the main thread, which keeps reading stdin. CI measured a cancel at about 150 ms. A query that cannot be cancelled in time is handled by the
driver killing the process, which is the fallback the plan names.

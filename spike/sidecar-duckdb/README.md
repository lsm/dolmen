# The DuckDB sidecar (spike)

A minimal sidecar: it registers one namespace's Iceberg table at a pinned snapshot and runs caller
SQL, reading the protocol in [../driver/PROTOCOL.md](../driver/PROTOCOL.md). It exists to be measured
and attacked, not to be the final shape — see that document for what the protocol does not decide.

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

Two things this replaces, both recorded in the plan as assumptions that turned out to be wrong:

- **No `make GEN=ninja EXTENSIONS='iceberg;httpfs'`.** A prebuilt library plus prebuilt extensions
  is enough, so there is no 40-minute DuckDB build per platform and no `ccache` to carry.
- **A fully static Linux build is available, not merely hoped for.** The `-musl` release zips ship
  `libduckdb_static.a` (83.9 MB at v1.5.6) for both `linux_amd64` and `linux_arm64`. The
  `sidecar-packaging` job links it and reports whether the result is genuinely static.

A source build remains the fallback if a platform ever lacks a prebuilt extension, and that fallback
is a toolchain-plus-long-build cost the release pipeline would then carry.

## Confinement

The settings are written into a `.duckdbrc` before the database starts, because
`enable_external_access` cannot be set once the process is up. The order is measured, not guessed:

1. `allowed_directories` — the one namespace data directory.
2. `extension_directory` — where the pre-placed extensions live.
3. `enable_external_access = false` — the guard; it cannot be set after (1).
4. `autoinstall_known_extensions` / `autoload_known_extensions` = false.
5. `allow_persistent_secrets` = false.
6. `lock_configuration = true` — last, so nothing above can be reopened.

This is the lock #533 measured. `memory_limit` is *not* set here: once `lock_configuration` is on a
`SET` is refused, so the ceiling is passed through `DBConfig::options.maximum_memory` at open time
instead.

`SIDECAR_UNLOCKED=1` starts the sidecar with no `.duckdbrc` at all. That is what lets the driver
tell a refusal that is *our mechanism* from one that is merely an engine default: the same statement
is run against both starts and compared.

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

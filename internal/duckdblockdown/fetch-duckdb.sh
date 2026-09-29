#!/bin/sh
# Download the pinned DuckDB CLI and verify its checksum.
#
# usage: ASSET=duckdb_cli-linux-amd64.zip fetch-duckdb.sh <dest-dir>
#
# The version and the checksums are the two things that make the lockdown
# reproducible. The spike's result is only as good as the version it was measured
# on, and confinement that silently changes on an upgrade is the failure mode
# this exists to prevent, so the checksum is refused rather than warned about.
#
# The digests are the ones the DuckDB release publishes for $VERSION, recorded
# here so a build does not have to trust the network to be self-consistent.
set -eu

DEST="${1:?usage: ASSET=<name> fetch-duckdb.sh <dest-dir>}"
VERSION="v1.5.6"

case "${ASSET:-}" in
duckdb_cli-linux-amd64.zip)   WANT=6e89deac1ebbc36eed0291caf8b567b030c7b86ac35998f71854e22b3c5d5e2f ;;
duckdb_cli-linux-arm64.zip)   WANT=c544e92c9b7c31fc53c2139802cabd8e2d1b2b3e3f933117f31611239c1402db ;;
duckdb_cli-osx-amd64.zip)     WANT=ae74c8cd74304bde1d92d941aca37bc72084ee8daa5660913819d8a9e9d29331 ;;
duckdb_cli-osx-arm64.zip)     WANT=8e0f6825653f8d057922e6147db920bebf072cb41f4b041fd35521c18d7d126e ;;
duckdb_cli-windows-amd64.zip) WANT=798eae475d07c645ff3b914f7b0e676d06502b5412dd7552638fafed81f9e916 ;;
duckdb_cli-windows-arm64.zip) WANT=266052dbf513da86d0d90209db09ec56e79af75b7fbe2d952810526dfc5d2726 ;;
*) echo "fetch-duckdb: set ASSET to one of the pinned duckdb_cli-<platform>.zip names" >&2; exit 1 ;;
esac

mkdir -p "$DEST"
ZIP="$DEST/$ASSET"
URL="https://github.com/duckdb/duckdb/releases/download/$VERSION/$ASSET"

echo "fetching $URL"
curl -sSLf -o "$ZIP" "$URL"

GOT=$(sha256sum "$ZIP" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$ZIP" | cut -d' ' -f1)
if [ "$GOT" != "$WANT" ]; then
  echo "fetch-duckdb: $ASSET sha256 is $GOT, want $WANT" >&2
  rm -f "$ZIP"
  exit 1
fi
echo "$ASSET sha256 verified"

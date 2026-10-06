#!/bin/sh
# Build dolmen-duckdb for one platform and package it as a release bundle:
# the sidecar, the pinned DuckDB library beside it, and the pinned extensions
# in duckdb-extensions/. Unpacked next to the dolmen binary, -engine lakehouse
# finds both without flags.
#
# usage: package.sh <platform> <version> <out-dir>
#   platform: linux-amd64 | linux-arm64 | darwin-arm64 | windows-amd64
set -eu

PLATFORM="${1:?usage: package.sh <platform> <version> <out-dir>}"
VERSION="${2:?usage: package.sh <platform> <version> <out-dir>}"
OUT="${3:?usage: package.sh <platform> <version> <out-dir>}"
HERE=$(cd "$(dirname "$0")" && pwd)

case "$PLATFORM" in
linux-amd64)   asset=libduckdb-linux-amd64.zip   ext=linux_amd64   lib=libduckdb.so    bin=dolmen-duckdb ;;
linux-arm64)   asset=libduckdb-linux-arm64.zip   ext=linux_arm64   lib=libduckdb.so    bin=dolmen-duckdb ;;
darwin-arm64)  asset=libduckdb-osx-universal.zip ext=osx_arm64     lib=libduckdb.dylib bin=dolmen-duckdb ;;
windows-amd64) asset=libduckdb-windows-amd64.zip ext=windows_amd64 lib=duckdb.dll      bin=dolmen-duckdb.exe ;;
*) echo "package.sh: unknown platform $PLATFORM" >&2; exit 1 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
"$HERE/fetch-deps.sh" lib "$asset" "$work/lib"
"$HERE/fetch-deps.sh" ext "$ext" "$work/ext"

name="dolmen-duckdb-$VERSION-$PLATFORM"
stage="$work/$name"
mkdir -p "$stage/duckdb-extensions"
case "$PLATFORM" in
windows-*)
  cl /nologo /std:c++17 /EHsc /O2 "/I$work/lib" "/Fe:$stage/$bin" "$HERE/dolmen-duckdb.cpp" "$work/lib/duckdb.lib" /link "/LIBPATH:$work/lib"
  rm -f "$stage"/*.obj dolmen-duckdb.obj
  ;;
darwin-*)
  c++ -std=c++17 -O2 -I"$work/lib" "$HERE/dolmen-duckdb.cpp" -L"$work/lib" -lduckdb -Wl,-rpath,@loader_path -o "$stage/$bin"
  ;;
*)
  g++ -std=c++17 -O2 -I"$work/lib" "$HERE/dolmen-duckdb.cpp" -L"$work/lib" -lduckdb -Wl,-rpath,'$ORIGIN' -o "$stage/$bin"
  ;;
esac
cp "$work/lib/$lib" "$stage/"
cp -R "$work/ext/." "$stage/duckdb-extensions/"
cp "$HERE/PROTOCOL.md" "$stage/"

mkdir -p "$OUT"
tar -C "$work" -czf "$OUT/$name.tar.gz" "$name"
echo "$OUT/$name.tar.gz"

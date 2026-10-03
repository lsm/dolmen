#!/bin/sh
# Download the DuckDB library or extensions the sidecar spike builds against,
# over HTTPS, and refuse anything whose sha256 is not the pinned one.
#
# usage: fetch-sidecar-deps.sh lib <libduckdb-asset.zip> <dest-dir>
#        fetch-sidecar-deps.sh ext <extension-platform> <dest-dir>
#
# The extensions are native code loaded into the sidecar, so they get the same
# treatment as internal/duckdblockdown/fetch-duckdb.sh gives the CLI: a pinned
# version and a pinned digest. The library digests are the ones GitHub publishes
# for the v1.5.6 release assets; extensions.duckdb.org publishes none, so those
# were recorded from an HTTPS download of each .gz.
set -eu

VERSION="v1.5.6"
KIND="${1:?usage: fetch-sidecar-deps.sh lib|ext <name> <dest-dir>}"
NAME="${2:?usage: fetch-sidecar-deps.sh lib|ext <name> <dest-dir>}"
DEST="${3:?usage: fetch-sidecar-deps.sh lib|ext <name> <dest-dir>}"

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

fetch() {
  url="$1" out="$2" want="$3"
  echo "fetching $url"
  curl -sSLf --proto '=https' --tlsv1.2 -o "$out" "$url"
  got=$(sha256 "$out")
  if [ "$got" != "$want" ]; then
    echo "fetch-sidecar-deps: $out sha256 is $got, want $want" >&2
    rm -f "$out"
    exit 1
  fi
  echo "$(basename "$out") sha256 verified"
}

ext_digest() {
  case "$1/$2" in
  linux_amd64/iceberg)   echo ed140567c919269f2a175fed5f0455c707d7af6489459d46f32ae9e4362f0076 ;;
  linux_amd64/httpfs)    echo 19e6906934a845487c96f9c94beee250c71e32bb9be260eb27ad96939a1df5f0 ;;
  linux_arm64/iceberg)   echo 62dfc3a9a82c6da8cf9a727bd641f0eb83f7a227885419ecd7ebcc74ac3dac64 ;;
  linux_arm64/httpfs)    echo b18472a0e85cbf15ca4ebbe335173f87b62a074cd0f26072d4c756205c6239d0 ;;
  osx_arm64/iceberg)     echo 48766dd86177c1e4b163618b9d8f81639e9907ec0d5513fc309fdf6f4445a3cb ;;
  osx_arm64/httpfs)      echo b9f9ca8e64d913a80d6666a370e0e518ee4cc8ecb9990a2b9d2273204d8caad4 ;;
  windows_amd64/iceberg) echo e4efc0d244258761cbb8671d88c98864379157a7bb690735e85217f0b7708188 ;;
  windows_amd64/httpfs)  echo e23aa882afd8aa24dacb8290920fec03b5188bb60038bdf6b963035a93d989cf ;;
  *) echo "fetch-sidecar-deps: no pinned digest for $2 on $1" >&2; exit 1 ;;
  esac
}

mkdir -p "$DEST"
case "$KIND" in
lib)
  case "$NAME" in
  libduckdb-linux-amd64.zip)      WANT=b845005f5132a7d8180057c35e14a7626632258782f871a90861b19c1c03841b ;;
  libduckdb-linux-arm64.zip)      WANT=b72ed9f05003f5e9d2015f7ceada6416b377d9dd33169cdde3c9e33897856eee ;;
  libduckdb-osx-universal.zip)    WANT=e0bc007d9b0094c0970ac1847a8601d10aad07cbd2910ce586ec810f77b638d6 ;;
  libduckdb-windows-amd64.zip)    WANT=44cf59583f9951d2cb09b1bf115a63ecb2d8901e363903029d86c7d8683fe96a ;;
  libduckdb-linux-amd64-musl.zip) WANT=9b82ed3720135c9821f5b8fa83744500bdc0146d3f88cdbc86dace35f0249e70 ;;
  libduckdb-linux-arm64-musl.zip) WANT=a568750e1bd07d7e1d9dfe78ed89bb6ea4d945a66c5302799f39987890eea57b ;;
  *) echo "fetch-sidecar-deps: no pinned digest for $NAME" >&2; exit 1 ;;
  esac
  fetch "https://github.com/duckdb/duckdb/releases/download/$VERSION/$NAME" "$DEST/$NAME" "$WANT"
  unzip -q -o "$DEST/$NAME" -d "$DEST"
  ;;
ext)
  for ext in iceberg httpfs; do
    fetch "https://extensions.duckdb.org/$VERSION/$NAME/$ext.duckdb_extension.gz" \
      "$DEST/$ext.duckdb_extension.gz" "$(ext_digest "$NAME" "$ext")"
    gunzip -f "$DEST/$ext.duckdb_extension.gz"
    test -f "$DEST/$ext.duckdb_extension"
  done
  ;;
*)
  echo "fetch-sidecar-deps: the first argument is lib or ext" >&2
  exit 1
  ;;
esac

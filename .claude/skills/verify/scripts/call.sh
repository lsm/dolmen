#!/usr/bin/env bash
# POST one operation to the server in a serve.sh work dir.
# Usage: call.sh <work dir> <op> '<json body>'
set -euo pipefail
curl -sS "$(cat "$1/url")/v1/$2" -H 'Content-Type: application/json' -d "$3"
echo

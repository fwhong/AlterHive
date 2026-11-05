#!/bin/sh
set -eu
ROOT=${1:-$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)}
exec python3 "$ROOT/scripts/verify-split.py"

#!/usr/bin/env bash
# Regenerate docs/demo*.gif from the real binary on synthetic fixtures. Needs Pillow (pip install pillow).
set -euo pipefail
cd "$(dirname "$0")/../.."
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
go build -o "$TMP/sesh" .
python3 scripts/demo/fixtures.py "$TMP/fx"
${PYTHON:-python3} scripts/demo/render.py "$TMP/sesh" "$TMP/fx" docs

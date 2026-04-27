#!/usr/bin/env bash
# lint-imports-ts.sh — enforces import-graph boundaries in the Parapet TS source.
#
# Rules:
#   No .ts/.tsx file outside parapet/src/sdk/overseer/ and parapet/src/gen/
#   may import from gen/overlord/v1/{events,overseer,adapter_plugin}_* directly.
#
# Exit code 0 = clean; 1 = violations found; 2 = usage error.

set -euo pipefail

REPO_ROOT="${1:-$(git -C "$(dirname "$0")" rev-parse --show-toplevel)}"
PARAPET_SRC="$REPO_ROOT/parapet/src"

if [[ ! -d "$PARAPET_SRC" ]]; then
  echo "error: could not find parapet/src at $PARAPET_SRC" >&2
  exit 2
fi

VIOLATIONS=0

# Pattern: imports from gen/overlord/v1/{events,overseer,adapter_plugin}_*
# These are only allowed inside parapet/src/sdk/overseer/ or parapet/src/gen/.
FORBIDDEN_PATTERN="from ['\"].*gen/overlord/v1/(events_pb|overseer_pb|overseer_connect|adapter_plugin_pb|adapter_plugin_connect)"

while IFS= read -r file; do
  # Compute path relative to parapet/src for exemption checks.
  rel="${file#$PARAPET_SRC/}"

  # Exempt: the shim itself and the gen files.
  if [[ "$rel" == sdk/overseer/* || "$rel" == gen/* ]]; then
    continue
  fi

  # Search for forbidden import patterns in this file.
  while IFS= read -r match; do
    lineno="${match%%:*}"
    content="${match#*:}"
    echo "$rel:$lineno: forbidden direct gen import — use parapet/src/sdk/overseer/ instead (${content// /})"
    VIOLATIONS=$((VIOLATIONS + 1))
  done < <(grep -nE "$FORBIDDEN_PATTERN" "$file" || true)
done < <(find "$PARAPET_SRC" -type f \( -name "*.ts" -o -name "*.tsx" \))

if [[ $VIOLATIONS -gt 0 ]]; then
  echo ""
  echo "import-lint-ts: $VIOLATIONS forbidden import(s) found." >&2
  exit 1
fi

echo "import-lint-ts: OK"

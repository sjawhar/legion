#!/usr/bin/env bash
# post-tool-use.sh: Auto-snapshot working copy after file changes
set -euo pipefail

# Read hook input
INPUT=$(cat)

# Check if jq is available
if ! command -v jq &>/dev/null; then
  exit 0
fi

TOOL_NAME=$(printf '%s' "$INPUT" | jq -r '.tool_name // empty')

# Only run after Edit or Write tools
case "$TOOL_NAME" in
  Edit|Write)
    # jj status triggers automatic working copy snapshot
    jj status >/dev/null 2>&1 || true
    ;;
esac

exit 0

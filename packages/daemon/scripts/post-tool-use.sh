#!/usr/bin/env bash
# post-tool-use.sh: Auto-snapshot jj working copy after file changes
set -euo pipefail

# jj status triggers automatic working copy snapshot
jj status >/dev/null 2>&1 || true

exit 0

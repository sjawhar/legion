#!/usr/bin/env bash
set -euo pipefail

host="${1:?usage: sync-host.sh user@host}"

PLUGIN_DIR="legion/default/packages/envoy-plugin"
PLUGIN_REF="file://{env:HOME}/${PLUGIN_DIR}"
REPO="sjawhar/legion"

# Find latest envoy release tag
tag=$(gh release list --repo "$REPO" --limit 10 --json tagName \
  --jq '[.[] | select(.tagName | startswith("legion-envoy-"))][0].tagName')
if [ -z "$tag" ]; then
  echo "ERROR: No legion-envoy release found" >&2
  exit 1
fi
echo "Using release: $tag"

# Download plugin tarball
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

gh release download "$tag" \
  --repo "$REPO" \
  --pattern '*.tgz' \
  --dir "$tmpdir" \
  --clobber

tgz=$(find "$tmpdir" -name '*.tgz' -print -quit)
if [ -z "$tgz" ]; then
  echo "ERROR: No .tgz found in release $tag" >&2
  exit 1
fi
echo "Downloaded: $(basename "$tgz")"

# Install on remote: extract tarball (strip package/ prefix from npm pack output)
ssh "$host" "rm -rf ~/${PLUGIN_DIR} && mkdir -p ~/${PLUGIN_DIR}"
scp -q "$tgz" "$host:/tmp/envoy-plugin.tgz"
ssh "$host" "tar xzf /tmp/envoy-plugin.tgz --strip-components=1 -C ~/${PLUGIN_DIR} && rm /tmp/envoy-plugin.tgz"
echo "Installed plugin to ~/${PLUGIN_DIR}"

# Update opencode.json: replace npm package reference with file:// path
# Resolves symlinks so we don't clobber dotfiles symlink structure
ssh "$host" "
  CONFIG=\$(readlink -f \$HOME/.config/opencode/opencode.json 2>/dev/null || echo \$HOME/.config/opencode/opencode.json)
  if [ -f \"\$CONFIG\" ]; then
    jq --arg ref '$PLUGIN_REF' \\
      '(.plugin // []) |= [.[] | if (test(\"opencode-legion-envoy\") and (test(\"^file://\") | not)) then \$ref else . end]' \\
      \"\$CONFIG\" > /tmp/opencode.json.tmp && mv /tmp/opencode.json.tmp \"\$CONFIG\"
    echo \"Updated opencode.json: \$CONFIG\"
  else
    echo \"WARNING: opencode.json not found at \$CONFIG — add plugin manually: $PLUGIN_REF\"
  fi
"

# Update tui.json: ensure file:// ref is present in the TUI plugin list.
# tui.json is a separate config file from opencode.json; opencode loads TUI
# plugins (slash commands, sidebar slots) only from here.
ssh "$host" "
  TUI_CONFIG=\$(readlink -f \$HOME/.config/opencode/tui.json 2>/dev/null || echo \$HOME/.config/opencode/tui.json)
  if [ -f \"\$TUI_CONFIG\" ]; then
    jq --arg ref '$PLUGIN_REF' \\
      '(.plugin // []) |= ((map(if (test(\"opencode-legion-envoy\") and (test(\"^file://\") | not)) then \$ref else . end)) | if any(. == \$ref) then . else . + [\$ref] end)' \\
      \"\$TUI_CONFIG\" > /tmp/tui.json.tmp && mv /tmp/tui.json.tmp \"\$TUI_CONFIG\"
    echo \"Updated tui.json: \$TUI_CONFIG\"
  else
    mkdir -p \$(dirname \"\$TUI_CONFIG\")
    jq -n --arg ref '$PLUGIN_REF' '{\"\\\$schema\":\"https://opencode.ai/tui.json\",\"plugin\":[\$ref]}' > \"\$TUI_CONFIG\"
    echo \"Created tui.json: \$TUI_CONFIG\"
  fi
"

echo "Done: $host envoy-plugin synced from release $tag"

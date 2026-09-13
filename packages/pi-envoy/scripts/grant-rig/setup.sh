#!/bin/sh
# Lays out everything the grant rig needs: a throwaway Oh My Pi profile that loads the Legion
# extension, and a scratch state directory that stands in for the daemon's `state_dir`. See
# README.md for what each piece is for.
#
# usage: setup.sh <checkout under test> [rig dir]
# env:   RIG_PLUGINS         unset      -> extension-only: the two extensions symlinked from the
#                                          checkout, no plugins directory
#                            production -> a copy of the real `legion` profile's plugin tree
#                                          (secretsd included) with only the Legion plugin swapped
#        RIG_LEGION_BUILD    branch (default) | a released version such as 1.17.1 — which Legion
#                            plugin build production mode installs
#        RIG_REFRESH_PLUGINS 1 -> re-copy the plugin tree even if the rig profile already has one
#        RIG_PROFILE         rig profile name (l12rig); RIG_SOURCE_PROFILE the copied one (legion)
# prints: RIG=<rig dir> RIG_PLUGINS=<mode> RIG_LEGION_BUILD=<build> on success
set -eu

SRC=$(realpath "$1")
RIG=${2:-$(mktemp -d /tmp/l12rig.XXXX)}
PROFILE=${RIG_PROFILE:-l12rig}
PROFILE_DIR="$HOME/.omp/profiles/$PROFILE"
AGENT="$PROFILE_DIR/agent"
SOURCE_PROFILE_DIR="$HOME/.omp/profiles/${RIG_SOURCE_PROFILE:-legion}"
PLUGINS_MODE=${RIG_PLUGINS:-extension-only}
LEGION_BUILD=${RIG_LEGION_BUILD:-branch}

# This script removes and rewrites the rig profile's plugins/ and agent/extensions/. It must never
# be the profile it copies from: only the operator edits ~/.omp/profiles/legion. Checked first,
# before any filesystem action, by canonical path (a symlinked alias is the same profile).
if [ "$(realpath -m -- "$PROFILE_DIR")" = "$(realpath -m -- "$SOURCE_PROFILE_DIR")" ]; then
  echo "refusing: rig profile equals source profile ($PROFILE_DIR is $SOURCE_PROFILE_DIR); set RIG_PROFILE to a scratch profile name" >&2
  exit 2
fi

test -f "$SRC/packages/pi-envoy/extensions/legion.ts" || {
  echo "not a legion checkout: $SRC" >&2
  exit 2
}
test -d "$SRC/node_modules" || {
  echo "run 'bun install --frozen-lockfile' in $SRC first" >&2
  exit 2
}
case "$PLUGINS_MODE" in extension-only | production) ;; *)
  echo "RIG_PLUGINS must be unset or 'production' (got '$PLUGINS_MODE')" >&2
  exit 2
  ;;
esac

# Profile: production model roles and task isolation, copied from the source profile.
mkdir -p "$AGENT"
cp "$SOURCE_PROFILE_DIR/agent/config.yml" "$SOURCE_PROFILE_DIR/agent/models.yml" "$AGENT/"

BUILD_COMMIT=-
if [ "$PLUGINS_MODE" = production ]; then
  # Exactly one copy of each extension loads: the plugin tree's, never a symlinked source copy.
  rm -rf "$AGENT/extensions"
  # The real profile's whole plugin tree (~240 MB): secretsd — whose bash-tool replacement is what
  # this mode exists to include — and every other production plugin, at the pinned versions.
  # Copied once; a later production run reuses it unless RIG_REFRESH_PLUGINS=1.
  if [ ! -d "$PROFILE_DIR/plugins/node_modules" ] || [ "${RIG_REFRESH_PLUGINS:-0}" = 1 ]; then
    rm -rf "$PROFILE_DIR/plugins"
    cp -a "$SOURCE_PROFILE_DIR/plugins" "$PROFILE_DIR/plugins"
  fi
  PKG="$PROFILE_DIR/plugins/node_modules/@sjawhar/pi-legion-envoy"
  test -f "$PKG/package.json" || {
    echo "the copied plugin tree has no @sjawhar/pi-legion-envoy: $PKG" >&2
    exit 2
  }
  # Swap only the Legion plugin.
  if [ "$LEGION_BUILD" = branch ]; then
    # The checkout's build over the installed dist/. The installed package.json stays: its
    # omp.extensions already names dist/envoy.js and dist/legion.js.
    (cd "$SRC/packages/pi-envoy" && bun run build >/dev/null)
    cp "$SRC/packages/pi-envoy/dist/envoy.js" "$SRC/packages/pi-envoy/dist/legion.js" "$PKG/dist/"
    BUILD_COMMIT=$(jj -R "$SRC" log -r @ --no-graph -T commit_id 2>/dev/null || echo unknown)
  else
    # A released build, exactly as `bun add @sjawhar/pi-legion-envoy@<version>` would install it.
    mkdir -p "$RIG/pkgs"
    TARBALL=$(cd "$RIG/pkgs" && npm pack "@sjawhar/pi-legion-envoy@$LEGION_BUILD" --silent)
    rm -rf "$PKG"
    mkdir -p "$PKG"
    tar -xzf "$RIG/pkgs/$TARBALL" --strip-components=1 -C "$PKG"
  fi
else
  # No plugins directory, so exactly one copy of each extension loads: the checkout's, symlinked.
  rm -rf "$PROFILE_DIR/plugins"
  mkdir -p "$AGENT/extensions"
  ln -sfn "$SRC/packages/pi-envoy/extensions/envoy.ts" "$AGENT/extensions/envoy.ts"
  ln -sfn "$SRC/packages/pi-envoy/extensions/legion.ts" "$AGENT/extensions/legion.ts"
fi

# Scratch state directory: what the daemon's `state_dir` holds for a pane.
mkdir -p "$RIG/state/bin" "$RIG/state/secrets" "$RIG/state/gh" "$RIG/ws"
chmod 0700 "$RIG/state/secrets"
printf 'rig-boot\n' > "$RIG/state/secrets/boot"
chmod 0600 "$RIG/state/secrets/boot"

# The `gh` shim, installed the way the daemon installs it at startup. A checkout without the
# module (main before LEGION-54) gets none here: its 1.17.x extensions wrote the shim themselves.
if test -f "$SRC/packages/daemon/src/daemon/worker-bin.ts"; then
  bun "$SRC/packages/daemon/src/daemon/worker-bin.ts" "$RIG/state" >/dev/null
fi

# The checkout's own `legion` command-line tool, the way `<state_dir>/bin/legion` re-execs the
# daemon's runtime in production.
cat > "$RIG/state/bin/legion" <<EOF
#!/bin/sh
exec "$(command -v bun)" "$SRC/packages/daemon/src/cli/index.ts" "\$@"
EOF
chmod 0755 "$RIG/state/bin/legion"

# Appends what the calling shell command actually ran under, so the worker prompt never has to
# name the credential: the grant file's contents and mode (file delivery), then the plain
# variable (text delivery on the 1.17.0 build; the field the before runs' commands ran under).
# `-` marks an absent value.
cat > "$RIG/state/bin/record-grant" <<EOF
#!/bin/sh
f=\${LEGION_GRANT_FILE:-}
g=-
m=-
if [ -n "\$f" ] && [ -r "\$f" ]; then
  g=\$(cat -- "\$f")
  m=\$(stat -c %a -- "\$f")
fi
printf '%s %s %s\\n' "\$g" "\$m" "\${LEGION_GRANT:--}" >> "$RIG/seen-grants.log"
EOF
chmod 0755 "$RIG/state/bin/record-grant"

# The worker's workspace: `bootstrapWorker` sets a jj identity on it.
if ! test -d "$RIG/ws/.jj"; then
  jj git init "$RIG/ws" >/dev/null
fi

# What this rig runs, for report.json.
cat > "$RIG/rig-mode.json" <<EOF
{"plugins":"$PLUGINS_MODE","legionBuild":"$LEGION_BUILD","commit":"$BUILD_COMMIT","checkout":"$SRC"}
EOF

: > "$RIG/standin.log"
: > "$RIG/seen-grants.log"
echo "RIG=$RIG RIG_PLUGINS=$PLUGINS_MODE RIG_LEGION_BUILD=$LEGION_BUILD"

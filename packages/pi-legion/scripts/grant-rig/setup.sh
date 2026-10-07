#!/bin/sh
# Lays out everything the grant rig needs: a throwaway Oh My Pi profile that loads the Envoy and
# Legion extensions, and a scratch state directory that stands in for the daemon's `state_dir`. See
# README.md for what each piece is for.
#
# usage: setup.sh <checkout under test> [rig dir]
# env:   RIG_PLUGINS         unset      -> extension-only: the two extensions symlinked from the
#                                          checkout, no plugins directory
#                            production -> a copy of the real `legion` profile's plugin tree
#                                          (secretsd included) with only the two Legion plugins
#                                          (@sjawhar/pi-envoy, @sjawhar/pi-legion) swapped
#        RIG_LEGION_BUILD    branch (default) | a released version such as 8.0.0 — which build of
#                            the two plugins production mode installs: branch packs the checkout's
#                            (scripts/e2e/lib/pack-plugin.sh), a version is `npm pack`ed from the
#                            registry; each tarball is extracted in the installed plugin's place
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

test -f "$SRC/packages/pi-legion/extensions/legion.ts" || {
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
  for NAME in pi-envoy pi-legion; do
    PKG="$PROFILE_DIR/plugins/node_modules/@sjawhar/$NAME"
    test -f "$PKG/package.json" || {
      echo "the copied plugin tree has no @sjawhar/$NAME: $PKG" >&2
      exit 2
    }
  done
  # Swap only the two Legion plugins, each for a tarball extracted in its place: what `npm pack`
  # ships (dist/ with the bundle and dist/skills, the Legion plugin's agents/, package.json),
  # nothing kept from the installed release. The rest of the copied tree stays, secretsd and the
  # other plugins included; neither package needs any of it, since its bundle inlines every
  # dependency except the @oh-my-pi/* packages Oh My Pi itself provides.
  rm -rf "$RIG/pkgs"
  mkdir -p "$RIG/pkgs/pi-envoy" "$RIG/pkgs/pi-legion"
  if [ "$LEGION_BUILD" = branch ]; then
    # The checkout's own plugins, each packed by the one pack step the live proofs use, as the
    # release packs it.
    test -f "$SRC/scripts/e2e/lib/pack-plugin.sh" || {
      echo "the checkout has no scripts/e2e/lib/pack-plugin.sh to pack its plugins with: $SRC" >&2
      exit 2
    }
    ENVOY_TARBALL=$(bash "$SRC/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$RIG/pkgs/pi-envoy")
    LEGION_TARBALL=$(bash "$SRC/scripts/e2e/lib/pack-plugin.sh" pi-legion "$RIG/pkgs/pi-legion")
    BUILD_COMMIT=$(jj -R "$SRC" log -r @ --no-graph -T commit_id 2>/dev/null || echo unknown)
  else
    # Released builds, exactly as `bun add @sjawhar/pi-envoy@<version> @sjawhar/pi-legion@<version>`
    # would install them; both release from one commit at one version.
    ENVOY_TARBALL="$RIG/pkgs/pi-envoy/$(cd "$RIG/pkgs/pi-envoy" && npm pack "@sjawhar/pi-envoy@$LEGION_BUILD" --silent)"
    LEGION_TARBALL="$RIG/pkgs/pi-legion/$(cd "$RIG/pkgs/pi-legion" && npm pack "@sjawhar/pi-legion@$LEGION_BUILD" --silent)"
  fi
  for NAME in pi-envoy pi-legion; do
    PKG="$PROFILE_DIR/plugins/node_modules/@sjawhar/$NAME"
    case "$NAME" in pi-envoy) TARBALL=$ENVOY_TARBALL ;; *) TARBALL=$LEGION_TARBALL ;; esac
    rm -rf "$PKG"
    mkdir -p "$PKG"
    tar -xzf "$TARBALL" --strip-components=1 -C "$PKG"
  done
else
  # No plugins directory, so exactly one copy of each extension loads: the checkout's, symlinked.
  rm -rf "$PROFILE_DIR/plugins"
  mkdir -p "$AGENT/extensions"
  ln -sfn "$SRC/packages/pi-envoy/extensions/envoy.ts" "$AGENT/extensions/envoy.ts"
  ln -sfn "$SRC/packages/pi-legion/extensions/legion.ts" "$AGENT/extensions/legion.ts"
fi

# Scratch state directory: what the daemon's `state_dir` holds for a pane.
mkdir -p "$RIG/state/secrets" "$RIG/state/gh" "$RIG/ws"
chmod 0700 "$RIG/state/secrets"
printf 'rig-boot\n' > "$RIG/state/secrets/boot"
chmod 0600 "$RIG/state/secrets/boot"

# The checkout's own Go `legion`, built beside the state directory as the daemon's own binary is.
test -f "$SRC/packages/daemon/cmd/legion/main.go" || {
  echo "the checkout has no Go legion to build (packages/daemon/cmd/legion): $SRC" >&2
  exit 2
}
(cd "$SRC/packages/daemon" && go build -o "$RIG/legion" ./cmd/legion)

# worker-bin/gh and bin/legion, installed by the daemon's own boot step (workerbin.Install, run
# through daemon-pane.ts): the `gh` shim drops its own directory from PATH and execs `legion gh`,
# and the launcher execs the daemon's binary, here the one just built.
WORKER_BIN=$(bun "$SRC/packages/pi-legion/scripts/grant-rig/daemon-pane.ts" "$SRC/packages/daemon" install "$RIG/state" "$RIG/legion")

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

# What this rig runs, for report.json, and the gh shim's directory, which verdict G expects first
# on the pane's PATH.
cat > "$RIG/rig-mode.json" <<EOF
{"plugins":"$PLUGINS_MODE","legionBuild":"$LEGION_BUILD","commit":"$BUILD_COMMIT","checkout":"$SRC","workerBin":"$WORKER_BIN"}
EOF

: > "$RIG/standin.log"
: > "$RIG/seen-grants.log"
echo "RIG=$RIG RIG_PLUGINS=$PLUGINS_MODE RIG_LEGION_BUILD=$LEGION_BUILD"

#!/bin/sh
# Lays out everything the grant rig needs: a throwaway Oh My Pi profile that loads the Legion
# extension straight from a source checkout, and a scratch state directory that stands in for
# the daemon's `state_dir`. See README.md for what each piece is for.
#
# usage: setup.sh <checkout under test> [rig dir]
# prints: RIG=<rig dir> on success
set -eu

SRC=$(realpath "$1")
RIG=${2:-$(mktemp -d /tmp/l12rig.XXXX)}
PROFILE=${RIG_PROFILE:-l12rig}
AGENT="$HOME/.omp/profiles/$PROFILE/agent"
SOURCE_PROFILE="$HOME/.omp/profiles/${RIG_SOURCE_PROFILE:-legion}/agent"

test -f "$SRC/packages/pi-envoy/extensions/legion.ts" || {
  echo "not a legion checkout: $SRC" >&2
  exit 2
}
test -d "$SRC/node_modules" || {
  echo "run 'bun install --frozen-lockfile' in $SRC first" >&2
  exit 2
}

# Profile: production model roles and task isolation, no plugins directory (so exactly one
# copy of each extension loads), the two extensions linked from the checkout under test.
mkdir -p "$AGENT/extensions"
cp "$SOURCE_PROFILE/config.yml" "$SOURCE_PROFILE/models.yml" "$AGENT/"
ln -sfn "$SRC/packages/pi-envoy/extensions/envoy.ts" "$AGENT/extensions/envoy.ts"
ln -sfn "$SRC/packages/pi-envoy/extensions/legion.ts" "$AGENT/extensions/legion.ts"

# Scratch state directory.
mkdir -p "$RIG/state/bin" "$RIG/state/secrets" "$RIG/ws"
chmod 0700 "$RIG/state/secrets"
printf 'rig-boot\n' > "$RIG/state/secrets/boot"
chmod 0600 "$RIG/state/secrets/boot"

# The branch's own `legion` command-line tool, the way `<state_dir>/bin/legion` re-execs the
# daemon's runtime in production.
cat > "$RIG/state/bin/legion" <<EOF
#!/bin/sh
exec "$(command -v bun)" "$SRC/packages/daemon/src/cli/index.ts" "\$@"
EOF
chmod 0755 "$RIG/state/bin/legion"

# Appends the credential the calling shell command actually ran under, so the worker prompt
# never has to name the variable.
cat > "$RIG/state/bin/record-grant" <<EOF
#!/bin/sh
printf '%s\\n' "\$LEGION_GRANT" >> "$RIG/seen-grants.log"
EOF
chmod 0755 "$RIG/state/bin/record-grant"

# The worker's workspace: `bootstrapWorker` sets a jj identity on it.
if ! test -d "$RIG/ws/.jj"; then
  jj git init "$RIG/ws" >/dev/null
fi

: > "$RIG/standin.log"
: > "$RIG/seen-grants.log"
echo "RIG=$RIG"

#!/usr/bin/env bash
# Routes a named OMP profile's model turns to the Hawk model gateway (middleman) on the operator's
# own identity, so a tmux stage proof's panes reach Anthropic with no provider key: the route every
# devbox agent session uses (~/.omp/agent/models.yml: `X-Api-Key: !hawk-token`).
#
#   scripts/e2e/lib/install-model-gateway.sh --profile <name> --dest <dir>
#
# Stdout is one line, the key command's path; every refusal goes to stderr. It writes:
#   <dir>/hawk-token      the key command the profile names: hawk-token, run with the caller's
#                         session bus address and XDG base directories for that one command
#   <dir>/hawk-token.log  one line per invocation, then hawk-token's own stderr
#   the profile's agent/models.yml and agent/config.yml (see the heredocs below)
#
# A pane cannot run hawk-token itself. Its XDG base directories are the daemon's own, under
# <state_dir>/home (LEGION-206 P1), so mise finds no global config and the `uv` hawk-token runs has
# no version; and it carries no DBUS_SESSION_BUS_ADDRESS, the only address the keyring client reads
# (jeepney/bus.py find_session_bus), so the hawk login is out of reach. Handing the key command
# both, and only it, keeps every pane's environment as it is: panes run as the operator's uid and
# can reach /run/user/<uid>/bus anyway, so the command gains nothing a pane lacks.
#
# Run it before the caller moves an XDG directory of its own (the stage proofs point
# XDG_STATE_HOME at their work directory): the key command takes the caller's values at this
# moment, and a variable the caller has unset stays unset for it. Its first mint, here, is the
# preflight: a locked keyring (every reboot locks it; the unlock-keyring skill) is refused by name
# before any pane exists, and hawk-token's periodic self-refresh, which can outlast OMP's ten-second
# budget for a `!command`, runs now rather than inside a pane's first model call.
set -euo pipefail

me=install-model-gateway
# The production gateway, the one hawk-token's default HAWK_API_URL mints for.
gateway=https://middleman.hawk.internal.trajectorylabs.com/anthropic
model=anthropic/claude-opus-4-8

# refuse is for arguments (exit 2); fail is for the box the run is on (exit 1).
refuse() {
  echo "$me: $*" >&2
  exit 2
}
fail() {
  echo "$me: $*" >&2
  exit 1
}

profile=
dest=
have_profile=
have_dest=
while [ $# -gt 0 ]; do
  case "$1" in
  --profile)
    [ $# -ge 2 ] || refuse "--profile needs a value: the OMP profile to route"
    profile=$2 have_profile=1
    shift 2
    ;;
  --dest)
    [ $# -ge 2 ] || refuse "--dest needs a value: the directory the key command is written to"
    dest=$2 have_dest=1
    shift 2
    ;;
  *) refuse "unknown argument: $1 (usage: $0 --profile <name> --dest <dir>)" ;;
  esac
done
[ -n "$have_profile" ] || refuse "--profile is required: the OMP profile to route"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the key command is written to"

# OMP reads an empty name or "default" as the profile every plain `omp` uses (the same rule as
# install-plugin-profile.sh).
trimmed=${profile#"${profile%%[![:space:]]*}"}
trimmed=${trimmed%"${trimmed##*[![:space:]]}"}
if [ -z "$trimmed" ] || [ "$trimmed" = default ]; then
  refuse "--profile '$profile' is OMP's default profile, the one plain \`omp\` uses; name a dedicated profile"
fi
agent=$HOME/.omp/profiles/$profile/agent
for file in models.yml config.yml; do
  [ ! -e "$agent/$file" ] || refuse "$agent/$file exists; this installs a fresh profile's model route only"
done

hawk_token=$(command -v hawk-token) ||
  fail "hawk-token is not on PATH: it mints the gateway key from the operator's hawk login"
hawk_token=$(realpath -- "$hawk_token")
[ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ] ||
  fail "DBUS_SESSION_BUS_ADDRESS is unset: hawk-token reads the hawk login from the keyring over the session bus"

dest=$(realpath -m -- "$dest")
# The path is written into YAML as an OMP `!command`, so it stays one plain word.
case "$dest" in *[!A-Za-z0-9/._-]*) refuse "--dest $dest holds a character a plain \`!command\` word cannot" ;; esac
if [ -e "$dest" ] && { [ ! -d "$dest" ] || [ -n "$(ls -A -- "$dest")" ]; }; then
  refuse "--dest $dest exists and is not an empty directory"
fi
mkdir -p "$dest"
chmod 0700 "$dest"
key_command=$dest/hawk-token
log=$dest/hawk-token.log

# env takes every -u before any assignment.
unsets=()
sets=("DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS")
for name in XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME XDG_RUNTIME_DIR; do
  if [ -n "${!name+set}" ]; then sets+=("$name=${!name}"); else unsets+=(-u "$name"); fi
done
{
  cat <<'EOF'
#!/bin/bash
# Written by scripts/e2e/lib/install-model-gateway.sh: hawk-token under the operator's session bus
# and XDG base directories, for this one command. Stdout is the gateway key.
EOF
  printf 'log=%q\n' "$log"
  words=$(printf '%q ' "${unsets[@]}" "${sets[@]}" "$hawk_token")
  printf 'command=(%s)\n' "${words% }"
  cat <<'EOF'
printf '%s invoked by pid %s\n' "$(date -u +%FT%TZ)" "$PPID" >>"$log"
exec /usr/bin/env "${command[@]}" 2>>"$log"
EOF
} >"$key_command"
chmod 0700 "$key_command"

# The preflight mint: the value is checked for a JWT's shape and never printed.
status=0
key=$("$key_command") || status=$?
if [ "$status" != 0 ] || ! [[ "$key" =~ ^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$ ]]; then
  why=$(grep -v '^[0-9TZ:-]* invoked by pid' "$log" | grep -v '^[[:space:]]*$' | tail -1 || true)
  unset key
  if grep -qi keyring "$log"; then
    fail "the operator's keyring is locked, so hawk-token cannot read the hawk login: unlock it (the unlock-keyring skill) and rerun. hawk-token: $why"
  fi
  fail "$key_command (hawk-token) exited $status without a gateway key; hawk-token: ${why:-no output} (log: $log)"
fi
unset key

mkdir -p "$agent"
cat >"$agent/models.yml" <<EOF
# Written by scripts/e2e/lib/install-model-gateway.sh: anthropic through the Hawk model gateway,
# keyed by the operator's hawk login. The gateway reads the key from x-api-key; OMP caches the
# command's value for the process and runs it again on a 401.
providers:
  anthropic:
    baseUrl: $gateway
    apiKey: "!$key_command"
    headers:
      X-Api-Key: "!$key_command"
EOF
cat >"$agent/config.yml" <<EOF
# Written by scripts/e2e/lib/install-model-gateway.sh. The gateway is the one model route, and Oh
# My Pi falls back from a failing provider without a word: with the key command failing, a pane
# answered from amazon-bedrock/us.anthropic.claude-opus-4-8 on the devbox's instance role, and a
# Stage 3 retro's scout subagent ran on Bedrock's openai.gpt-oss-120b. enabledModels holds every
# session's own model to anthropic, so a pane without the key refuses to start. Subagents and
# retries choose from every enabled provider instead, so each one a pane can use without the
# gateway is disabled: Bedrock twice (the instance role is its key), Google (Stage 2's provider-key
# path hands panes GEMINI_API_KEY), and the local servers OMP uses with no key. Every role is the
# one model: the gateway answers claude-haiku-4-5, which OMP gave that scout next, with 404.
enabledModels:
  - anthropic/*
disabledProviders:
  - amazon-bedrock
  - bedrock-mantle
  - google
  - ollama
  - llama.cpp
  - lm-studio
modelRoles:
  default: $model
  smol: $model
  slow: $model
  vision: $model
  plan: $model
  commit: $model
  tiny: $model
  task: $model
  advisor: $model
EOF
echo "$me: OMP profile $profile routes $model through $gateway, keyed by $key_command" >&2
printf '%s\n' "$key_command"

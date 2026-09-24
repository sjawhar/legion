#!/usr/bin/env bash
# Routes a named OMP profile's model turns to the Hawk model gateway (middleman) on the operator's
# own identity, so a tmux stage proof's panes reach Anthropic with no provider key: the route every
# devbox agent session uses (~/.omp/agent/models.yml: `X-Api-Key: !hawk-token`).
#
#   scripts/e2e/lib/install-model-gateway.sh --profile <name> --dest <dir> --cache-dir <dir>
#
# Stdout is one line, the key command's path; every refusal goes to stderr. It writes:
#   <dir>/hawk-token      the key command the profile names: hawk-token, run with the caller's
#                         session bus address and XDG base directories for that one command
#   <dir>/hawk-token.log  one line per invocation, one per mint, and hawk-token's own stderr
#   <cache-dir>/hawk-token.key   the minted key, 0600, kept until shortly before it expires
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
cache_dir=
have_profile=
have_dest=
have_cache_dir=
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
  --cache-dir)
    [ $# -ge 2 ] || refuse "--cache-dir needs a value: the private directory the minted key is kept in"
    cache_dir=$2 have_cache_dir=1
    shift 2
    ;;
  *) refuse "unknown argument: $1 (usage: $0 --profile <name> --dest <dir> --cache-dir <dir>)" ;;
  esac
done
[ -n "$have_profile" ] || refuse "--profile is required: the OMP profile to route"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the key command is written to"
[ -n "$have_cache_dir" ] || refuse "--cache-dir is required: the private directory the minted key is kept in"

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
[ -n "$cache_dir" ] || refuse "--cache-dir is empty"
cache_dir=$(realpath -m -- "$cache_dir")
mkdir -p "$cache_dir"
chmod 0700 "$cache_dir"

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
#
# Each hawk-token run reads the hawk login from the keyring over the session bus, and the devbox's
# keyring daemon has died serving such a read, relocking the keyring until the operator unlocks it.
# So the key is minted once and kept, 0600, until 300 seconds before its JWT exp (for 300 seconds
# when it has none). Oh My Pi runs this once per process and again after a 401, so a second call
# from a process already given the kept key means the gateway refused it: that call mints afresh.
EOF
  printf 'log=%q\n' "$log"
  printf 'cache=%q\n' "$cache_dir/hawk-token.key"
  printf 'served=%q\n' "$cache_dir/hawk-token.served"
  words=$(printf '%q ' "${unsets[@]}" "${sets[@]}" "$hawk_token")
  printf 'command=(%s)\n' "${words% }"
  cat <<'EOF'
margin=300
window=300
set -o pipefail
umask 077
now=$(date +%s)
printf '%s invoked by pid %s\n' "$(date -u +%FT%TZ)" "$PPID" >>"$log"
if [ -s "$cache" ] && ! grep -qx -- "$PPID" "$served" 2>/dev/null; then
  read -r kept_until key <"$cache"
  if [ "$now" -lt "$kept_until" ]; then
    printf '%s\n' "$PPID" >>"$served"
    printf '%s\n' "$key"
    exit 0
  fi
fi
key=$(/usr/bin/env "${command[@]}" 2>>"$log") || exit
payload=${key#*.}
payload=${payload%%.*}
payload=$(printf '%s' "$payload" | tr '_-' '/+')
while [ $((${#payload} % 4)) != 0 ]; do payload+='='; done
exp=$(printf '%s' "$payload" | base64 -d 2>/dev/null | jq -r '.exp // empty | numbers | floor' 2>/dev/null) || exp=
if [ -n "$exp" ]; then kept_until=$((exp - margin)); else kept_until=$((now + window)); fi
printf '%s %s\n' "$kept_until" "$key" >"$cache.tmp" && mv -f "$cache.tmp" "$cache"
printf '%s\n' "$PPID" >"$served"
printf '%s minted a key for pid %s, kept until %s\n' "$(date -u +%FT%TZ)" "$PPID" "$(date -u -d "@$kept_until" +%FT%TZ)" >>"$log"
printf '%s\n' "$key"
EOF
} >"$key_command"
chmod 0700 "$key_command"

# The preflight mint: the value is checked for a JWT's shape and never printed.
status=0
key=$("$key_command") || status=$?
if [ "$status" != 0 ] || ! [[ "$key" =~ ^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$ ]]; then
  why=$(grep -v -e '^[0-9TZ:-]* invoked by pid' -e '^[0-9TZ:-]* minted a key for pid' "$log" | grep -v '^[[:space:]]*$' | tail -1 || true)
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

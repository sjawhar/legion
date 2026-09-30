#!/usr/bin/env bash
# Routes a named OMP profile's model turns to the Hawk model gateway (middleman) on the operator's
# own identity, so a tmux stage proof's panes reach Anthropic with no provider key: the route every
# devbox agent session uses (~/.omp/agent/models.yml: `X-Api-Key: !hawk-token`).
#
#   scripts/e2e/lib/install-model-gateway.sh --profile <name> --home <dir> --dest <dir> --cache-dir <dir>
#
# --home is the HOME Oh My Pi runs under for the profile, made by make_omp_home (lib/omp-home.sh): the
# profile's files go under <home>/.omp/profiles/<name>, and the caller's own HOME is refused.
#
# LEGION_E2E_MODEL_GATEWAY_URL is required: the gateway's Anthropic endpoint, the one hawk-token's
# default HAWK_API_URL mints for, checked by lib/model-gateway-url.sh.
#
# Stdout is one line, the key command's path; every refusal goes to stderr. It writes:
#   <dir>/hawk-token      the key command the profile names: hawk-token, run with the caller's
#                         HOME, session bus address and XDG base directories for that one command
#   <dir>/hawk-token.log  one line per invocation, one per mint, one per call it served no key,
#                         and hawk-token's own stderr
#   <dir>/hawk-token.unserved   one line per call it served no key and why, read by
#                         lib/model-gateway-unserved.sh; absent while every call has been served
#   <cache-dir>/hawk-token.key   the minted key, 0600, kept until shortly before it expires
#   <cache-dir>/hawk-token.key.lock   the lock one call holds while it mints
#   the profile's agent/models.yml and agent/config.yml (see the heredocs below)
#
# A pane cannot run hawk-token itself. Its XDG base directories are the daemon's own, under
# <state_dir>/home (LEGION-206 P1), and its HOME is the run's own (make_omp_home), so mise finds no
# global config and the `uv` hawk-token runs has no version; and it carries no
# DBUS_SESSION_BUS_ADDRESS, the only address the keyring client reads (jeepney/bus.py
# find_session_bus), so the hawk login is out of reach. Handing the key command all of them, and
# only it, keeps every pane's environment as it is: panes run as the operator's uid and can reach
# /run/user/<uid>/bus anyway, so the command gains nothing a pane lacks.
#
# Run it under the operator's own HOME, before the caller moves an XDG directory of its own (the
# stage proofs point XDG_STATE_HOME at their work directory): the key command takes the caller's
# values at this moment, and a variable the caller has unset stays unset for it. Its first mint,
# here, is the preflight: a locked keyring (every reboot locks it; the unlock-keyring skill) is
# refused by name before any pane exists, and hawk-token's periodic self-refresh, which can outlast
# OMP's ten-second budget for a `!command`, runs now rather than inside a pane's first model call.
set -euo pipefail

me=install-model-gateway
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
home=
dest=
cache_dir=
have_profile=
have_home=
have_dest=
have_cache_dir=
while [ $# -gt 0 ]; do
  case "$1" in
  --profile)
    [ $# -ge 2 ] || refuse "--profile needs a value: the OMP profile to route"
    profile=$2 have_profile=1
    shift 2
    ;;
  --home)
    [ $# -ge 2 ] || refuse "--home needs a value: the HOME Oh My Pi runs under for the profile"
    home=$2 have_home=1
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
  *) refuse "unknown argument: $1 (usage: $0 --profile <name> --home <dir> --dest <dir> --cache-dir <dir>)" ;;
  esac
done
[ -n "$have_profile" ] || refuse "--profile is required: the OMP profile to route"
[ -n "$have_home" ] || refuse "--home is required: the HOME Oh My Pi runs under for the profile"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the key command is written to"
[ -n "$have_cache_dir" ] || refuse "--cache-dir is required: the private directory the minted key is kept in"

# OMP reads an empty name or "default" as the profile every plain `omp` uses (the same rule as
# install-plugin-profile.sh).
trimmed=${profile#"${profile%%[![:space:]]*}"}
trimmed=${trimmed%"${trimmed##*[![:space:]]}"}
if [ -z "$trimmed" ] || [ "$trimmed" = default ]; then
  refuse "--profile '$profile' is OMP's default profile, the one plain \`omp\` uses; name a dedicated profile"
fi
# The profile goes under the run's own home, never the caller's (the same rule, and the same
# refusal of an unset or empty HOME, as install-plugin-profile.sh).
[ -n "${HOME:-}" ] || refuse "HOME is unset or empty, so --home cannot be checked against it"
home=$(realpath -m -- "$home")
[ "$home" != "$(realpath -m -- "$HOME")" ] || refuse "--home $home is your own HOME; give the profile a home of its own (make_omp_home, lib/omp-home.sh)"
[ -d "$home/.omp" ] || refuse "--home $home has no .omp directory; make it with make_omp_home (lib/omp-home.sh)"
agent=$home/.omp/profiles/$profile/agent
for file in models.yml config.yml; do
  [ ! -e "$agent/$file" ] || refuse "$agent/$file exists; this installs a fresh profile's model route only"
done

hawk_token=$(command -v hawk-token) ||
  fail "hawk-token is not on PATH: it mints the gateway key from the operator's hawk login"
hawk_token=$(realpath -- "$hawk_token")
# One call mints at a time under flock, and a call gives up before Oh My Pi kills it under timeout;
# both are named by path in the key command, which runs on a pane's PATH.
flock=$(command -v flock) || fail "flock is not on PATH: the key command holds a lock while it mints"
timeout=$(command -v timeout) || fail "timeout is not on PATH: the key command bounds its mint by it"
[ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ] ||
  fail "DBUS_SESSION_BUS_ADDRESS is unset: hawk-token reads the hawk login from the keyring over the session bus"
# The helper names the variable in its own refusal.
gateway=$(bash "$(dirname -- "${BASH_SOURCE[0]}")/model-gateway-url.sh") || exit 1

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
# A key already there would be served without a mint, so the directory starts empty.
if [ -e "$cache_dir" ] && { [ ! -d "$cache_dir" ] || [ -n "$(ls -A -- "$cache_dir")" ]; }; then
  refuse "--cache-dir $cache_dir exists and is not an empty directory"
fi
mkdir -p "$cache_dir"
chmod 0700 "$cache_dir"

# env takes every -u before any assignment.
unsets=()
sets=("HOME=$HOME" "DBUS_SESSION_BUS_ADDRESS=$DBUS_SESSION_BUS_ADDRESS")
for name in XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME XDG_RUNTIME_DIR; do
  if [ -n "${!name+set}" ]; then sets+=("$name=${!name}"); else unsets+=(-u "$name"); fi
done
{
  cat <<'EOF'
#!/bin/bash
# Written by scripts/e2e/lib/install-model-gateway.sh: hawk-token under the operator's HOME, session
# bus and XDG base directories, for this one command. Stdout is the gateway key.
#
# Each hawk-token run reads the hawk login from the keyring over the session bus, and the devbox's
# keyring daemon has died serving such a read, relocking the keyring until the operator unlocks it.
# So the key is minted once and kept, 0600, until 300 seconds before its JWT exp (for 300 seconds
# when it has none), and every call in that window gets it. A key the gateway refuses early is not
# re-minted: the proof's model turns fail, loudly.
#
# One call mints at a time. A wave of agents that starts as the kept key expires calls at once, and
# when each minted its own, four of five mints on a loaded devbox ran past hawk-token's 9000 ms
# budget and their agents started with no key. So a call that finds no kept key takes the lock
# beside the cache and looks again, minting only when the key is still missing; the rest wait on
# the lock and serve the key it kept.
#
# Oh My Pi kills a `!command` 10 s after starting it, and a killed call records nothing, so a call
# gives up at deadline_ms, waiting or minting. Each call that gets no key appends one line to the
# unserved record: time, caller pid, caller working directory, reason and detail, tab-separated.
# The reason is timeout when the call ran out of time (its own deadline, or hawk-token's budget)
# and failed when the mint ended without a key before then (lib/model-gateway-unserved.sh). A
# caller whose environment names MODEL_GATEWAY_UNSERVED_FILE gets its line there too, so a harness
# that gives each agent its own file can read one run's verdict from that run's own directory.
EOF
  printf 'log=%q\n' "$log"
  printf 'cache=%q\n' "$cache_dir/hawk-token.key"
  printf 'record=%q\n' "$dest/hawk-token.unserved"
  printf 'flock=%q\n' "$flock"
  printf 'timeout=%q\n' "$timeout"
  words=$(printf '%q ' "${unsets[@]}" "${sets[@]}" "$hawk_token")
  printf 'command=(%s)\n' "${words% }"
  cat <<'EOF'
margin=300
window=300
deadline_ms=9500
set -o pipefail
umask 077
# The call's clock starts with its process, which is when Oh My Pi started the command (/bin/sh
# execs this file): on a loaded devbox bash took 580 ms to reach its first line. The clock is read
# without a fork, since each fork there costs tens of milliseconds more.
read -r stat </proc/self/stat
read -r -a fields <<<"${stat##*) }"
read -r uptime _ </proc/uptime
# uptime has two decimals; starttime, the 20th field after the name, is in USER_HZ ticks (100/s).
started_us=$((${EPOCHREALTIME/[.,]/} - (${uptime/./}0 - ${fields[19]}0) * 1000))
left_ms() { left=$((deadline_ms - (${EPOCHREALTIME/[.,]/} - started_us) / 1000)); }
TZ=UTC printf '%(%FT%TZ)T invoked by pid %s\n' -1 "$PPID" >>"$log"
# serve_kept ends the call with the kept key while the key is inside its window.
serve_kept() {
  local kept_until key
  [ -s "$cache" ] || return 0
  read -r kept_until key <"$cache"
  [ "$EPOCHSECONDS" -lt "$kept_until" ] || return 0
  printf '%s\n' "$key"
  exit 0
}
# unserved REASON DETAIL records that this call gets no key, and ends it.
unserved() {
  local at line detail=${2//[$'\t\n']/ }
  TZ=UTC printf -v at '%(%FT%TZ)T' -1
  printf -v line '%s\t%s\t%s\t%s\t%s' "$at" "$PPID" "${PWD//[$'\t\n']/ }" "$1" "$detail"
  printf '%s\n' "$line" >>"$record"
  [ -z "${MODEL_GATEWAY_UNSERVED_FILE:-}" ] || printf '%s\n' "$line" >>"$MODEL_GATEWAY_UNSERVED_FILE"
  printf '%s no key for pid %s (%s): %s\n' "$at" "$PPID" "$1" "$detail" >>"$log"
  exit 1
}
# PWD names the directory Oh My Pi ran the call in, physically.
cd -P . || exit
serve_kept
exec {lock}>>"$cache.lock"
left_ms
if [ "$left" -le 0 ]; then
  unserved timeout "reached the lock after $((deadline_ms - left)) ms, past the call's $deadline_ms ms"
fi
printf -v wait_s '%d.%03d' $((left / 1000)) $((left % 1000))
if ! "$flock" -w "$wait_s" "$lock"; then
  left_ms
  unserved timeout "waited $((deadline_ms - left)) ms for another call's mint, past the call's $deadline_ms ms"
fi
# The call that held the lock may have kept a key while this one waited.
serve_kept
left_ms
[ "$left" -gt 0 ] || unserved timeout "waited $((deadline_ms - left)) ms for another call's mint and took the lock with none of the call's $deadline_ms ms left"
printf -v mint_s '%d.%03d' $((left / 1000)) $((left % 1000))
# Only the call holding the lock writes err. The lock is this call's alone: hawk-token starts a
# detached refresh that would hold it for minutes.
err=$cache.err
status=0
key=$("$timeout" --kill-after=0.2 "$mint_s" /usr/bin/env "${command[@]}" 2>"$err" {lock}>&-) || status=$?
mapfile -t errlines <"$err"
[ "${#errlines[@]}" = 0 ] || printf '%s\n' "${errlines[@]}" >>"$log"
if [ "$status" != 0 ] || [ -z "$key" ]; then
  left_ms
  if [ "$status" = 124 ] || [ "$left" -le 0 ]; then
    unserved timeout "the mint ran past the call's $deadline_ms ms and was stopped (exit $status)"
  fi
  # hawk-token's own line names its budget when it gives up: in <spent> ms of a <budget> ms budget.
  why=
  for line in "${errlines[@]}"; do
    if [[ $line =~ in\ ([0-9]+)\ ms\ of\ a\ ([0-9]+)\ ms\ budget ]] && ((BASH_REMATCH[1] >= BASH_REMATCH[2])); then
      unserved timeout "$line"
    fi
    [[ ! $line =~ [^[:space:]] ]] || why=$line
  done
  unserved failed "hawk-token exited $status without a key: ${why:-no output}"
fi
payload=${key#*.}
payload=${payload%%.*}
payload=$(printf '%s' "$payload" | tr '_-' '/+')
while [ $((${#payload} % 4)) != 0 ]; do payload+='='; done
exp=$(printf '%s' "$payload" | base64 -d 2>/dev/null | jq -r '.exp // empty | numbers | floor' 2>/dev/null) || exp=
if [ -n "$exp" ]; then kept_until=$((exp - margin)); else kept_until=$((EPOCHSECONDS + window)); fi
printf '%s %s\n' "$kept_until" "$key" >"$cache.tmp" && mv -f "$cache.tmp" "$cache"
left_ms
TZ=UTC printf '%(%FT%TZ)T minted a key for pid %s in %s ms, kept until %(%FT%TZ)T\n' -1 "$PPID" "$((deadline_ms - left))" "$kept_until" >>"$log"
printf '%s\n' "$key"
EOF
} >"$key_command"
chmod 0700 "$key_command"

# The preflight mint: the value is checked for a JWT's shape and never printed.
status=0
key=$("$key_command") || status=$?
if [ "$status" != 0 ] || ! [[ "$key" =~ ^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$ ]]; then
  why=$(grep -v -e '^[0-9TZ:-]* invoked by pid' -e '^[0-9TZ:-]* minted a key for pid' -e '^[0-9TZ:-]* no key for pid' "$log" | grep -v '^[[:space:]]*$' | tail -1 || true)
  unset key
  if grep -qi keyring "$log"; then
    fail "the operator's keyring is locked, so hawk-token cannot read the hawk login: unlock it (the unlock-keyring skill) and rerun. hawk-token: $why"
  fi
  # The key command's own verdict, timeout or failed, names hawk-token's line with it.
  if [ -s "$dest/hawk-token.unserved" ]; then
    fail "$key_command got no gateway key: $(tail -1 "$dest/hawk-token.unserved" | cut -f4-5 --output-delimiter=': ') (log: $log)"
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
# one model: the gateway answers claude-haiku-4-5, which OMP gave that scout next, with 404. review
# and oracle are the roles Legion's task agents name (@review, @oracle), which the daemon's boot
# gate refuses to start without.
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
  review: $model
  oracle: $model
EOF
echo "$me: OMP profile $profile routes $model through LEGION_E2E_MODEL_GATEWAY_URL, keyed by $key_command" >&2
printf '%s\n' "$key_command"

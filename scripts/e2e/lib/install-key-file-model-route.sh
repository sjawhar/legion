#!/usr/bin/env bash
# Routes a named OMP profile's model turns straight to Anthropic's own endpoint, keyed by a local
# file: the key-file alternative to lib/install-model-gateway.sh's Hawk route, for a devbox that has
# no Hawk gateway at all (lib/install-model-gateway.sh's own header; a devbox reaching Anthropic
# directly runs this one instead). Mirrors the model-route pattern
# deploy/kubernetes/operator-route/models-key-file.yml gives Stage 4a's pods: an operator-supplied
# key, read fresh on every call, never minted and never written into the tree.
#
#   scripts/e2e/lib/install-key-file-model-route.sh --profile <name> --home <dir> --dest <dir> --key-dir <dir> --key-file <file>
#
# --home is the HOME Oh My Pi runs under for the profile, made by make_omp_home (lib/omp-home.sh):
# the profile's files go under <home>/.omp/profiles/<name>, and the caller's own HOME is refused.
#
# --key-file names a file holding the plaintext Anthropic API key alone (one line; a trailing
# newline is read past): refused unless it is a regular file only its owner can read (no group or
# other permission bit). The preflight below reads it once, before any pane exists, to prove it is
# readable; every later call reads it fresh again, so a key the caller rewrites in place (the same
# path, new content) takes effect on the key command's very next call — re-pointing --key-file's
# own path to a new file does nothing, since the key command is written with the resolved path
# baked in — though Oh My Pi itself caches the value for a running agent and only re-runs the
# command on a 401 (the profile's own models.yml comment has this).
#
# --dest and --key-dir hold different things, and the caller gives them different lifetimes:
# --dest gets only the call record (<dest>/anthropic-key.log, <dest>/anthropic-key.calls), which
# carries no secret and the stage proofs' own diagnostics (lib/model-gateway-unserved.sh --notes)
# read from a kept evidence directory after a failing run. --key-dir gets the key command itself
# (<key-dir>/anthropic-key), a script with --key-file's resolved path baked into it: anyone
# holding that script can read the key for as long as --key-file's path still holds it, so the
# caller must give --key-dir a path its own cleanup removes unconditionally, on every exit, not
# the evidence directory or a workspace a failing run keeps for debugging (the same rule
# lib/install-model-gateway.sh's hawk-token already follows for its own minted token file).
#
# Stdout is one line, the key command's path (under --key-dir); every refusal goes to stderr. It
# writes:
#   <key-dir>/anthropic-key  the key command the profile names: reads --key-file fresh, for that
#                            one command
#   <dest>/anthropic-key.log  one line per invocation and the file read's own failure, if any
#   <dest>/anthropic-key.calls   one line per call and its outcome, in lib/model-gateway-unserved.sh's
#                            own record shape, so it and the stage proofs that read a route's
#                            <key_command>.log/.calls read this route exactly as they read the
#                            gateway's
#   the profile's agent/models.yml and agent/config.yml (see the heredocs below)
#
# No key reaches a pane's own environment or the profile's models.yml, which names only the key
# command's path (baked in at install time): the key command alone is handed --key-file's path.
# A pane runs as the operator's own uid, though, so an agent's own bash can still read --key-file
# directly, or run the key command itself — the same exposure lib/install-model-gateway.sh's
# hawk-token has, not a widening. Unlike hawk-token there is nothing to mint and no login to lose
# to a relocked keyring, so there is no preflight budget and no cache: every call is a plain, fast
# file read, checked for its own permission bits once, here, before any pane exists, so a key file
# a group or other user can read is refused loudly at install rather than silently leaking the key
# to them on the first call.
set -euo pipefail

me=install-key-file-model-route
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
key_dir=
key_file=
have_profile=
have_home=
have_dest=
have_key_dir=
have_key_file=
while [ $# -gt 0 ]; do
  case $1 in
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
    [ $# -ge 2 ] || refuse "--dest needs a value: the directory the key command's call record is written to"
    dest=$2 have_dest=1
    shift 2
    ;;
  --key-dir)
    [ $# -ge 2 ] || refuse "--key-dir needs a value: the directory the key command's own script is written to"
    key_dir=$2 have_key_dir=1
    shift 2
    ;;
  --key-file)
    [ $# -ge 2 ] || refuse "--key-file needs a value: the file holding the plaintext Anthropic key"
    key_file=$2 have_key_file=1
    shift 2
    ;;
  *) refuse "unknown argument $1" ;;
  esac
done
[ -n "$have_profile" ] || refuse "--profile is required: the OMP profile to route"
[ -n "$have_home" ] || refuse "--home is required: the HOME Oh My Pi runs under for the profile"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the key command's call record is written to"
[ -n "$have_key_dir" ] || refuse "--key-dir is required: the directory the key command's own script is written to"
[ -n "$have_key_file" ] || refuse "--key-file is required: the file holding the plaintext Anthropic key"

# OMP reads an empty name or "default" as the profile every plain `omp` uses (the same rule as
# install-model-gateway.sh and install-plugin-profile.sh).
trimmed=${profile#"${profile%%[![:space:]]*}"}
trimmed=${trimmed%"${trimmed##*[![:space:]]}"}
if [ -z "$trimmed" ] || [ "$trimmed" = default ]; then
  refuse "--profile '$profile' is OMP's default profile, the one plain \`omp\` uses; name a dedicated profile"
fi
# The profile goes under the run's own home, never the caller's (the same rule as
# install-model-gateway.sh and install-plugin-profile.sh).
[ -n "${HOME:-}" ] || refuse "HOME is unset or empty, so --home cannot be checked against it"
home=$(realpath -m -- "$home")
[ "$home" != "$(realpath -m -- "$HOME")" ] || refuse "--home $home is your own HOME; give the profile a home of its own (make_omp_home, lib/omp-home.sh)"
[ -d "$home/.omp" ] || refuse "--home $home has no .omp directory; make it with make_omp_home (lib/omp-home.sh)"
agent=$home/.omp/profiles/$profile/agent
for file in models.yml config.yml; do
  [ ! -e "$agent/$file" ] || refuse "$agent/$file exists; this installs a fresh profile's model route only"
done

resolved_key_file=$(realpath -e -- "$key_file" 2>/dev/null) || refuse "--key-file $key_file does not exist"
key_file=$resolved_key_file
[ -f "$key_file" ] || refuse "--key-file $key_file is not a regular file"
perms=$(stat -c %a -- "$key_file") || refuse "--key-file $key_file: stat failed"
case $perms in
[0-7]00) ;;
*) refuse "--key-file $key_file is mode $perms: group or other can read it; chmod 600 it (it holds a plaintext Anthropic key)" ;;
esac

dest=$(realpath -m -- "$dest")
# The path is written into YAML as an OMP `!command`, so it stays one plain word.
case "$dest" in *[!A-Za-z0-9/._-]*) refuse "--dest $dest holds a character a plain \`!command\` word cannot" ;; esac
if [ -e "$dest" ] && { [ ! -d "$dest" ] || [ -n "$(ls -A -- "$dest")" ]; }; then
  refuse "--dest $dest exists and is not an empty directory"
fi
mkdir -p "$dest"
chmod 0700 "$dest"
log=$dest/anthropic-key.log

key_dir=$(realpath -m -- "$key_dir")
case "$key_dir" in *[!A-Za-z0-9/._-]*) refuse "--key-dir $key_dir holds a character a plain \`!command\` word cannot" ;; esac
case "$key_dir/" in
"$dest/") refuse "--key-dir $key_dir is --dest: the key command's own script, which bakes in --key-file's path, must not sit where the call record is kept" ;;
"$dest/"*) refuse "--key-dir $key_dir is inside --dest $dest: the key command's own script must not sit where the call record is kept" ;;
esac
case "$dest/" in
"$key_dir/"*) refuse "--dest $dest is inside --key-dir $key_dir: the call record must not sit where the key command's own script, which bakes in --key-file's path, is kept" ;;
esac
if [ -e "$key_dir" ] && { [ ! -d "$key_dir" ] || [ -n "$(ls -A -- "$key_dir")" ]; }; then
  refuse "--key-dir $key_dir exists and is not an empty directory"
fi
mkdir -p "$key_dir"
chmod 0700 "$key_dir"
key_command=$key_dir/anthropic-key

{
  cat <<'EOF'
#!/bin/bash
# Written by scripts/e2e/lib/install-key-file-model-route.sh: reads a plaintext Anthropic key from
# a fixed path, fresh, on every call. Stdout is the key.
#
# Every agent's call appends its outcome to the record, one tab-separated line: time, caller pid,
# caller working directory, agent, outcome and detail. What each field holds, and the values agent
# and outcome take, is stated once, in lib/model-gateway-unserved.sh's header; this route's own
# outcomes are only served and failed (a key file is never minted and never times out, so there is
# nothing for timeout or killed to mean here).
EOF
  printf 'log=%q\n' "$log"
  printf 'record=%q\n' "$dest/anthropic-key.calls"
  printf 'key_file=%q\n' "$key_file"
  cat <<'EOF'
set -o pipefail
umask 077
case "${1-}" in
"") preflight= ;;
--preflight) preflight=1 ;;
*)
  echo "anthropic-key key command: unknown argument $1 (Oh My Pi passes none; the installer passes --preflight)" >&2
  exit 2
  ;;
esac
# The call's clock starts with its own process: the profile names it `!exec <path>`, so the /bin/sh
# Oh My Pi starts execs this file, and its process start is that of Oh My Pi's call. Its PPID, the
# pid the log names, is the Oh My Pi process that ran the call (install-model-gateway.sh's own key
# command measured this the same way on 18.2.9: a child of the agent's own omp, not the agent's
# pid).
TZ=UTC printf '%(%FT%TZ)T invoked by pid %s\n' -1 "$PPID" >>"$log"
# PWD names the directory Oh My Pi ran the call in, physically.
cd -P . || exit
# The caller as the record's agent field names it (lib/model-gateway-unserved.sh's header).
agent=${LEGION_ROLE:-}${LEGION_ROLE:+${LEGION_GENERATION:+/$LEGION_GENERATION}}
agent=${agent:--}
# record OUTCOME DETAIL appends an agent's call to the record, and to the caller's
# MODEL_GATEWAY_CALLS_FILE when its environment names one. No field is empty: read splits on runs
# of tabs. The installer's preflight is no agent, so it is not recorded, the same rule
# install-model-gateway.sh's hawk-token key command follows.
record() {
  [ -z "$preflight" ] || return 0
  local at line detail=${2//[$'\t\n']/ }
  TZ=UTC printf -v at '%(%FT%TZ)T' -1
  printf -v line '%s\t%s\t%s\t%s\t%s\t%s' "$at" "$PPID" "${PWD//[$'\t\n']/ }" "${agent//[$'\t\n']/ }" "$1" "${detail:--}"
  printf '%s\n' "$line" >>"$record"
  [ -z "${MODEL_GATEWAY_CALLS_FILE:-}" ] || printf '%s\n' "$line" >>"$MODEL_GATEWAY_CALLS_FILE"
}
if key=$(<"$key_file"); then
  detail="empty"
else
  detail="could not read"
fi
if [ -z "${key:-}" ]; then
  record failed "$detail $key_file"
  TZ=UTC printf '%(%FT%TZ)T no key for pid %s: %s %s\n' -1 "$PPID" "$detail" "$key_file" >>"$log"
  echo "anthropic-key key command: no key (failed): $detail $key_file" >&2
  exit 1
fi
record served "read from $key_file"
printf '%s\n' "$key"
EOF
} >"$key_command"
chmod 0700 "$key_command"

# The preflight read: proves the file is readable before any pane exists, same spirit as
# install-model-gateway.sh's preflight mint, with nothing to cache and nothing that can time out.
status=0
key=$("$key_command" --preflight 2>"$key_dir/anthropic-key.preflight") || status=$?
reason=$(<"$key_dir/anthropic-key.preflight")
rm -f "$key_dir/anthropic-key.preflight"
if [ "$status" != 0 ]; then
  unset key
  fail "the preflight got no key from --key-file $key_file: ${reason:-$key_command exited $status and said nothing} (log: $log)"
fi
[ -n "$key" ] || fail "$key_file is empty (log: $log)"
unset key

mkdir -p "$agent"
cat >"$agent/models.yml" <<EOF
# Written by scripts/e2e/lib/install-key-file-model-route.sh: anthropic through its own endpoint,
# keyed from --key-file. OMP caches the command's value for the process and runs it again on a 401,
# so a key the operator rotates mid-run is picked up on the very next 401, not instantly. OMP runs a
# !command through /bin/sh -c, so exec makes the key command the process OMP started.
providers:
  anthropic:
    baseUrl: https://api.anthropic.com
    auth: apiKey
    api: anthropic-messages
    apiKey: "!exec $key_command"
    headers:
      X-Api-Key: "!exec $key_command"
EOF
cat >"$agent/config.yml" <<EOF
# Written by scripts/e2e/lib/install-key-file-model-route.sh. Anthropic's own endpoint is the one
# model route, and Oh My Pi falls back from a failing provider without a word: with the key command
# failing, install-model-gateway.sh measured a pane answering from
# amazon-bedrock/us.anthropic.claude-opus-4-8 on the devbox's instance role, and a Stage 3 retro's
# scout subagent on Bedrock's openai.gpt-oss-120b. enabledModels holds every session's own model to
# anthropic, so a pane without the key refuses to start. Subagents and retries choose from every
# enabled provider instead, so each one a pane can use without the key file is disabled: Bedrock
# twice (the instance role is its key), Google (Stage 2's provider-key path hands panes
# GEMINI_API_KEY), and the local servers OMP uses with no key. Every role is the one model. The
# daemon's boot gate refuses to start without the roles Legion's task agents name.
enabledModels:
  - $model
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
  deep: $model
# AskGate (pi-envoy, in every pane) reviews each Dispatch write inside one tool_call handler and
# holds its deadline 5 s under this limit. At Oh My Pi's 30 s default the deadline is 25 s, a review
# of a long session often times out, and a write whose review times out posts unreviewed
# (packages/pi-envoy/README.md, "The time limit").
extensionHandlers:
  toolCallTimeoutMs: 120000
EOF
echo "$me: OMP profile $profile routes $model straight to Anthropic, keyed by $key_command from $key_file" >&2
printf '%s\n' "$key_command"

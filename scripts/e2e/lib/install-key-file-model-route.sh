#!/usr/bin/env bash
# Routes a named OMP profile's model turns straight to Anthropic's own endpoint, keyed by a local
# file: the key-file alternative to lib/install-model-gateway.sh's Hawk route, for a devbox that has
# no Hawk gateway at all (lib/install-model-gateway.sh's own header; a devbox reaching Anthropic
# directly runs this one instead). Mirrors the model-route pattern
# deploy/kubernetes/operator-route/models-key-file.yml gives Stage 4a's pods: an operator-supplied
# key, read fresh on every call, never minted and never written into the tree.
#
#   scripts/e2e/lib/install-key-file-model-route.sh --profile <name> --home <dir> --dest <dir> --key-file <file>
#
# --home is the HOME Oh My Pi runs under for the profile, made by make_omp_home (lib/omp-home.sh):
# the profile's files go under <home>/.omp/profiles/<name>, and the caller's own HOME is refused.
#
# --key-file names a file holding the plaintext Anthropic API key alone (one line; a trailing
# newline is read past): refused unless it is a regular file only its owner can read (no group or
# other permission bit). Never read here beyond that permission check — each call reads it fresh,
# so a key the caller rotates mid-run takes effect on the very next call with no restart.
#
# Stdout is one line, the key command's path; every refusal goes to stderr. It writes:
#   <dir>/anthropic-key      the key command the profile names: reads --key-file fresh under the
#                            caller's HOME and XDG base directories, for that one command
#   <dir>/anthropic-key.log  one line per invocation and the file read's own failure, if any
#   <dir>/anthropic-key.calls   one line per call and its outcome, in lib/model-gateway-unserved.sh's
#                            own record shape, so it and the stage proofs that read a route's
#                            <key_command>.log/.calls read this route exactly as they read the
#                            gateway's
#   the profile's agent/models.yml and agent/config.yml (see the heredocs below)
#
# A pane cannot read an arbitrary path of the operator's choosing itself: the key command, and only
# it, is handed --key-file's path (baked into the written script at install time, never into a
# pane's own environment or the profile's models.yml, which names only the key command). Unlike
# hawk-token there is nothing to mint and no login to lose to a relocked keyring, so there is no
# preflight budget and no cache: every call is a plain, fast file read, checked for its own
# permission bits once, here, before any pane exists, so a key file a group or other user can read
# is refused loudly at install rather than silently leaking the key to them on the first call.
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
key_file=
have_profile=
have_home=
have_dest=
have_key_file=
while [ $# -gt 0 ]; do
  case $1 in
  --profile)
    profile=$2
    have_profile=1
    shift 2
    ;;
  --home)
    home=$2
    have_home=1
    shift 2
    ;;
  --dest)
    dest=$2
    have_dest=1
    shift 2
    ;;
  --key-file)
    key_file=$2
    have_key_file=1
    shift 2
    ;;
  *) refuse "unknown argument $1" ;;
  esac
done
[ -n "$have_profile" ] || refuse "--profile is required: the OMP profile to route"
[ -n "$have_home" ] || refuse "--home is required: the HOME Oh My Pi runs under for the profile"
[ -n "$have_dest" ] || refuse "--dest is required: the directory the key command is written to"
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

[ -f "$key_file" ] || refuse "--key-file $key_file is not a regular file"
perms=$(stat -c %a -- "$key_file") || refuse "--key-file $key_file: stat failed"
case $perms in
[0-7]00) ;;
*) refuse "--key-file $key_file is mode $perms: group or other can read it; chmod 600 it (it holds a plaintext Anthropic key)" ;;
esac
key_file=$(realpath -- "$key_file")

dest=$(realpath -m -- "$dest")
# The path is written into YAML as an OMP `!command`, so it stays one plain word.
case "$dest" in *[!A-Za-z0-9/._-]*) refuse "--dest $dest holds a character a plain \`!command\` word cannot" ;; esac
if [ -e "$dest" ] && { [ ! -d "$dest" ] || [ -n "$(ls -A -- "$dest")" ]; }; then
  refuse "--dest $dest exists and is not an empty directory"
fi
mkdir -p "$dest"
chmod 0700 "$dest"
key_command=$dest/anthropic-key
log=$dest/anthropic-key.log

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
key=$("$key_command" --preflight 2>"$dest/anthropic-key.preflight") || status=$?
reason=$(<"$dest/anthropic-key.preflight")
rm -f "$dest/anthropic-key.preflight"
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
EOF
echo "$me: OMP profile $profile routes $model straight to Anthropic, keyed by $key_command from $key_file" >&2
printf '%s\n' "$key_command"

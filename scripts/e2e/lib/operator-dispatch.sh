# shellcheck shell=bash disable=SC2034,SC2154 # the variables it sets are the caller's to read; $work is the caller's
# The operator's Dispatch, for a script run from the operator's shell. Sourced, never run.
#
# operator_dispatch resolves it as every command does, through lib/operator-dispatch.ts: the
# envoy.json `envoyConfigPath("command")` chooses (ENVOY_CONFIG_FILE, which the operator's own
# session sets, then the omp profile, Claude Code (only under CLAUDE_CODE_CHILD_SESSION=1) and
# OpenCode directories, then ${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json), the working
# directory's .envoy/envoy.json, and the environment as it is when the function is called, read by
# `dispatchSettings`: its URL and bearer, the same-server rule for a file's token, and a `!` token
# run. It sets operator_dispatch_file to the envoy.json that rule chose, and operator_dispatch_url
# and operator_dispatch_token to Dispatch's URL and bearer; when nothing configures Dispatch or a
# file turns it off, both are empty and operator_dispatch_notice says why. A file, a URL or a token
# that prevents Dispatch fails the call through the caller's `fail`, naming it. The bearer crosses
# from the TypeScript through a 0600 file in a private directory this function makes and removes,
# never stdout, and stays in this shell's unexported variable: it reaches no argv and no process's
# environment.
#
# A run that must not hand the operator's bearer to anything it starts (the Stage 3 scripts) calls
# operator_dispatch_hold_token once at start, and production_baseline reads its audit baseline from
# the operator's Dispatch with the bearer held (below).
operator_dispatch_lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)

operator_dispatch() {
  local resolved reason held status=0
  operator_dispatch_file='' operator_dispatch_url='' operator_dispatch_token='' operator_dispatch_notice=''
  held=$(mktemp -d) || fail "the operator's Dispatch: could not make a private directory for its bearer"
  resolved=$(bun "$operator_dispatch_lib/operator-dispatch.ts" "$held/bearer") || status=$?
  if [ -f "$held/bearer" ]; then operator_dispatch_token=$(<"$held/bearer"); fi
  rm -rf "$held"
  if [ "$status" -ne 0 ]; then
    reason=$(jq -r '.error // empty' <<<"$resolved" 2>/dev/null) || reason=''
    fail "the operator's Dispatch: ${reason:-lib/operator-dispatch.ts exited $status}"
  fi
  operator_dispatch_file=$(jq -r '.file' <<<"$resolved")
  operator_dispatch_url=$(jq -r '.url // empty' <<<"$resolved")
  operator_dispatch_notice=$(jq -r '.notice // empty' <<<"$resolved")
}

# operator_dispatch_hold_token takes the operator's DISPATCH_TOKEN out of the environment, into
# operator_token, so nothing the run starts inherits a production bearer; production_baseline hands
# it back to the one call that resolves the operator's Dispatch.
operator_dispatch_hold_token() {
  operator_token=${DISPATCH_TOKEN:-}
  unset DISPATCH_TOKEN
}

# prod_header_file writes the production bearer, prod_token, as a curl header to a 0600 file in the
# caller's $work and prints its path.
prod_header_file() {
  local header=$work/production-dispatch-auth
  (umask 077 && printf 'Authorization: Bearer %s\n' "$prod_token" >"$header")
  printf '%s\n' "$header"
}
# production_baseline reads a recent production Dispatch event id so the post-run audit replays
# only this run's window. It is a read-only query to the operator's Dispatch, resolved as every
# command of the operator's resolves it (operator_dispatch) with the DISPATCH_TOKEN the script took
# out of its environment at start (operator_dispatch_hold_token); a file, URL or token that prevents
# it fails the run, naming it. With no Dispatch configured there is no production Dispatch to audit.
# It sets prod_dispatch_url, prod_token and prod_baseline (`none` with no Dispatch), and says why
# through the caller's `note` when there is none.
production_baseline() {
  local header since keys key id max=0
  DISPATCH_TOKEN=$operator_token operator_dispatch
  if [ -z "$operator_dispatch_url" ]; then
    note "$operator_dispatch_notice"
    prod_baseline=none
    return 0
  fi
  prod_dispatch_url=$operator_dispatch_url prod_token=$operator_dispatch_token
  header=$(prod_header_file)
  since=$(date -u -d '-24 hours' +%Y-%m-%dT%H:%M:%SZ)
  keys=$(curl -fsS --max-time 20 -H "@$header" "$prod_dispatch_url/api/v1/issues?updated_since=$since" | jq -r '.[:50][].key')
  for key in $keys; do
    id=$(curl -fsS --max-time 20 -H "@$header" "$prod_dispatch_url/api/v1/issues/$key/events?order=desc&limit=1" | jq -r '.[0].id // 0')
    if [ "$id" -gt "$max" ]; then max=$id; fi
  done
  rm -f "$header"
  [ "$max" -gt 0 ] || fail "could not read a production Dispatch baseline event id"
  prod_baseline=$max
}

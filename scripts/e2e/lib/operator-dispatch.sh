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
# production_baseline reads a recent production Dispatch event id so the post-run audit reads every
# event from it on. It is a read-only query to the operator's Dispatch, resolved as every
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

# production_dispatch_audit SESSIONS ENVOY reads every production Dispatch event from prod_baseline
# on, whatever owns it (an issue, a document, a project or none), by paging GET /api/v1/events/log
# by id until a short page, and the production projects, into the caller's
# $evidence/production-dispatch-audit.json. It fails on any event a session in the JSON array
# SESSIONS authored or that names the caller's $project, and on a production project naming it. A
# Dispatch that answers the log route 404 is a build without it: the audit says so and reads the
# replay of that Dispatch's live socket instead (lib/dispatch-live-events.ts, resuming after
# baseline - 1 for 25 s), which also carries every event whatever owns it. Any other read that
# fails, or a read that does not carry the baseline event itself (the control that it covers the
# run's window), fails it too: the audit cannot vouch for production. Its summary line names the
# source it read and ENVOY, the rig sessions the caller found on the production Envoy listener.
production_dispatch_audit() {
  local sessions=$1 envoy=$2 out=$work/production-events.jsonl page=$work/production-events-page.json
  local header bearer status projects events read_from control
  local after=$((prod_baseline - 1)) complete=1 found=0
  header=$(prod_header_file)
  : >"$out"
  # The log route pages from the baseline on, so the baseline is its first event.
  read_from='GET /api/v1/events/log'
  control='.[0].id == $baseline'
  while :; do
    status=$(curl -sS --max-time 20 -o "$page" -w '%{http_code}' -H "@$header" \
      "$prod_dispatch_url/api/v1/events/log?after=$after&limit=1000") || status=
    if [ "$status" = 404 ] && [ "$after" -eq $((prod_baseline - 1)) ]; then
      printf 'production Dispatch has no GET /api/v1/events/log (a build without it); reading its /ws/live replay since %s instead\n' \
        "$after"
      read_from='the /ws/live replay'
      # A subscription starts at the resume it was sent or below it, never above, and Dispatch
      # replays nothing to a resume further back than its bound, so the baseline event need only
      # be among the events read.
      control='any(.[]; .id == $baseline)'
      bearer=$work/production-dispatch-bearer
      (umask 077 && printf '%s\n' "$prod_token" >"$bearer")
      # A refusal of the feed is printed on stderr; the replay's close, resubscribe and duplicate
      # lines carry no event, so an event a redial repeats is read once.
      bun "$operator_dispatch_lib/dispatch-live-events.ts" \
        --url "$prod_dispatch_url" --bearer-file "$bearer" --since "$after" --timeout 25 \
        >"$work/production-events.live.jsonl" || true
      rm -f "$bearer"
      jq -c '.event | select(. != null)' "$work/production-events.live.jsonl" >>"$out" || complete=
      break
    fi
    if [ "$status" != 200 ] || ! jq -c '.[]' "$page" >>"$out"; then
      complete=
      break
    fi
    [ "$(jq length "$page")" -eq 1000 ] || break
    after=$(jq '.[-1].id' "$page")
  done
  if [ -z "$complete" ] || ! projects=$(curl -fsS --max-time 20 -H "@$header" "$prod_dispatch_url/api/v1/projects"); then
    rm -f "$header"
    printf 'production Dispatch could not be read for the audit\n'
    return 1
  fi
  rm -f "$header"
  events=$(jq -s -c --argjson ids "$sessions" --arg project "$project" --argjson baseline "$prod_baseline" --argjson projects "$projects" '
    {control: ('"$control"'), count: length,
     rig: [.[] | select((.actor.id as $a | $ids | index($a)) or (tostring | contains($project)))
       | {id, issue_key, artifact_id, project, type, actor: .actor.id, created_at}],
     projects: [$projects[] | select(tostring | contains($project))]}' "$out")
  printf '%s\n' "$events" >"$evidence/production-dispatch-audit.json"
  if ! jq -e '.control' >/dev/null <<<"$events"; then
    printf 'production Dispatch read through %s did not carry its baseline event %s\n' "$read_from" "$prod_baseline"
    return 1
  fi
  if ! jq -e '.rig == []' >/dev/null <<<"$events"; then
    printf 'production Dispatch has rig-authored events: %s\n' "$(jq -c .rig <<<"$events")"
    found=1
  fi
  if ! jq -e '.projects == []' >/dev/null <<<"$events"; then
    printf 'production Dispatch holds the rig project: %s\n' "$(jq -c .projects <<<"$events")"
    found=1
  fi
  printf 'production audit: %s events from %s through %s, rig events %s, rig projects %s, rig Envoy sessions %s\n' \
    "$(jq -r .count <<<"$events")" "$prod_baseline" "$read_from" "$(jq -c .rig <<<"$events")" \
    "$(jq -c .projects <<<"$events")" "$envoy"
  return "$found"
}

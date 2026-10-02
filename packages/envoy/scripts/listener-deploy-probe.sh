#!/usr/bin/env bash
# listener-deploy-probe.sh — Watches an Envoy listener from a client's seat while its deployment
# rolls, and judges whether any task refused /v1 while it was in service. See
# packages/envoy/scripts/README.md for usage.
#
# A rolling deploy runs the old and the new task side by side, and a client that resolves the
# listener's name can reach either. Every tick the probe resolves the name (or takes the tasks'
# addresses from --targets) and asks each address, and the name itself, for /healthz and
# GET /v1/sessions; with --send-to it also sends a direct message, and with --dispatch-* it posts a
# Dispatch message every N ticks. It prints one tab-separated line per target per tick and a summary
# on exit, Ctrl-C included.
#
# Needs bash, curl, getent and sed only (no jq), so it also runs inside a listener task.
#
# Verdict: a target fails when /v1/sessions answers anything but 200 at a tick where its /healthz
# answered 200 (whatever its status, "starting" included), or when it never answers /v1/sessions 200
# at all. A /v1 request that times out, or whose connection closes or resets without an answer,
# counts as a non-200 answer. One that never connects right after /healthz answered (curl exit 7) is
# counted and shown, not judged a refusal: a listener that stops closes its listening socket first,
# so that is the task stopping between the tick's two requests. A target that never answers
# /healthz is unreached: named, not failed. With --dispatch-*, the run also fails when a Dispatch
# message did not record state sent: its delivery attempt failed, or Dispatch refused the post.
#
# Exit codes:
#   0 — at least one target answered /healthz, every target that did passed, and every Dispatch
#       message recorded state sent.
#   1 — a target failed, a Dispatch message did not record state sent, or no target ever answered
#       /healthz.
#   2 — usage error, or every /v1 answer of the run was 401 or 403 (the bearer, not the listener).
#   4 — a required tool is missing.
set -euo pipefail
export TZ=UTC

usage() {
  cat <<'EOF'
Usage: listener-deploy-probe.sh --url http://<name>:<port> [options]

Watch an Envoy listener's /healthz and /v1 at every address its name resolves to while its
deployment rolls, and report whether any task refused /v1 while it was in service.

Options:
  --url URL                  The listener's base URL, e.g. http://envoy-listener.internal.example:9020
                             (required).
  --targets IP[,IP...]       Probe these addresses instead of resolving the name each tick
                             (e.g. both tasks' addresses from aws ecs describe-tasks).
  --token-file PATH          File holding the listener's /v1 bearer (default: $ENVOY_TOKEN_FILE;
                             otherwise $ENVOY_TOKEN; otherwise no bearer).
  --interval SECONDS         Seconds between ticks (default 1).
  --duration SECONDS         Seconds to run, duration/interval ticks; 0 runs until Ctrl-C
                             (default 300).
  --send-to SESSION          Also POST /v1/messages/send to this session at every target each
                             tick, under one idempotency key per tick.
  --dispatch-url URL         Dispatch's base URL; with --dispatch-token-file, --dispatch-issue and
                             --dispatch-session, post an issue message targeting the session every
                             --dispatch-every ticks and record its delivery attempt's state; the
                             run fails unless every message records state sent.
  --dispatch-token-file PATH File holding a Dispatch bearer.
  --dispatch-issue KEY       The open issue the messages are posted on.
  --dispatch-session SESSION The live session the messages target.
  --dispatch-mode MODE       btw or steer (default btw).
  --dispatch-every N         Ticks between Dispatch messages (default 10).
  --help                     Show this help.

Output: one line per target per tick,
  ts target healthz_code healthz_status v1_code v1_error send_code dispatch_state
tab-separated, a code 000 when no whole answer came (v1_error then says why: no connection,
timeout, empty reply, connection reset), "-" for a column that does not apply, then a summary.
EOF
}

die_usage() {
  printf 'ERR: %s\n' "$1" >&2
  usage >&2
  exit 2
}

url=""
targets_arg=""
token_file="${ENVOY_TOKEN_FILE:-}"
interval=1
duration=300
send_to=""
dispatch_url=""
dispatch_token_file=""
dispatch_issue=""
dispatch_session=""
dispatch_mode="btw"
dispatch_every=10

while (($#)); do
  case "$1" in
    --help | -h)
      usage
      exit 0
      ;;
    --url | --targets | --token-file | --interval | --duration | --send-to | --dispatch-url | \
      --dispatch-token-file | --dispatch-issue | --dispatch-session | --dispatch-mode | --dispatch-every)
      (($# >= 2)) || die_usage "$1 needs a value"
      case "$1" in
        --url) url="$2" ;;
        --targets) targets_arg="$2" ;;
        --token-file) token_file="$2" ;;
        --interval) interval="$2" ;;
        --duration) duration="$2" ;;
        --send-to) send_to="$2" ;;
        --dispatch-url) dispatch_url="$2" ;;
        --dispatch-token-file) dispatch_token_file="$2" ;;
        --dispatch-issue) dispatch_issue="$2" ;;
        --dispatch-session) dispatch_session="$2" ;;
        --dispatch-mode) dispatch_mode="$2" ;;
        --dispatch-every) dispatch_every="$2" ;;
      esac
      shift 2
      ;;
    *) die_usage "unknown option: $1" ;;
  esac
done

[[ -n "$url" ]] || die_usage "--url is required"
url="${url%/}"
[[ "$url" =~ ^(https?)://([A-Za-z0-9.-]+)(:([0-9]+))?$ ]] || die_usage "--url must be http(s)://<host>[:<port>], got $url"
scheme="${BASH_REMATCH[1]}"
host="${BASH_REMATCH[2]}"
port="${BASH_REMATCH[4]}"
if [[ -z "$port" ]]; then
  port=80
  [[ "$scheme" == https ]] && port=443
fi
[[ "$interval" =~ ^[1-9][0-9]*$ ]] || die_usage "--interval must be a whole number of seconds >= 1"
[[ "$duration" =~ ^[0-9]+$ ]] || die_usage "--duration must be a whole number of seconds >= 0"
id_shape='^[A-Za-z0-9._:-]+$'
[[ -z "$send_to" || "$send_to" =~ $id_shape ]] || die_usage "--send-to is not a session id: $send_to"
targets=()
if [[ -n "$targets_arg" ]]; then
  IFS=, read -ra targets <<<"$targets_arg"
  for target in "${targets[@]}"; do
    [[ "$target" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || die_usage "--targets holds something that is not an IPv4 address: $target"
  done
fi
dispatching=0
if [[ -n "$dispatch_url$dispatch_token_file$dispatch_issue$dispatch_session" ]]; then
  [[ -n "$dispatch_url" && -n "$dispatch_token_file" && -n "$dispatch_issue" && -n "$dispatch_session" ]] ||
    die_usage "--dispatch-url, --dispatch-token-file, --dispatch-issue and --dispatch-session go together"
  dispatch_url="${dispatch_url%/}"
  [[ "$dispatch_url" =~ ^https?://[A-Za-z0-9.:-]+$ ]] || die_usage "--dispatch-url must be http(s)://<host>[:<port>], got $dispatch_url"
  [[ "$dispatch_issue" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || die_usage "--dispatch-issue is not an issue key: $dispatch_issue"
  [[ "$dispatch_session" =~ $id_shape ]] || die_usage "--dispatch-session is not a session id: $dispatch_session"
  [[ "$dispatch_mode" == btw || "$dispatch_mode" == steer ]] || die_usage "--dispatch-mode must be btw or steer"
  [[ "$dispatch_every" =~ ^[1-9][0-9]*$ ]] || die_usage "--dispatch-every must be a whole number >= 1"
  [[ -r "$dispatch_token_file" ]] || die_usage "cannot read --dispatch-token-file $dispatch_token_file"
  dispatching=1
fi
[[ -z "$token_file" || -r "$token_file" ]] || die_usage "cannot read the token file $token_file"

required=(curl sed)
[[ ${#targets[@]} -gt 0 ]] || required+=(getent)
for cmd in "${required[@]}"; do
  command -v "$cmd" >/dev/null || {
    printf 'ERR: missing required command: %s\n' "$cmd" >&2
    exit 4
  }
done

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# Bearers go to curl in header files, never on its command line, where ps shows them.
umask 077
envoy_header="${work_dir}/envoy.header"
token=""
if [[ -n "$token_file" ]]; then
  token="$(<"$token_file")"
else
  token="${ENVOY_TOKEN:-}"
fi
token="${token//[$'\r\n\t ']/}"
if [[ -n "$token" ]]; then
  printf 'Authorization: Bearer %s\n' "$token" >"$envoy_header"
else
  : >"$envoy_header"
fi
dispatch_header="${work_dir}/dispatch.header"
if ((dispatching)); then
  dispatch_token="$(<"$dispatch_token_file")"
  printf 'Authorization: Bearer %s\n' "${dispatch_token//[$'\r\n\t ']/}" >"$dispatch_header"
fi
body_file="${work_dir}/body"
exit_file="${work_dir}/exit"
# never_connected is no_answer's reason for a request that found nothing listening.
readonly never_connected="no connection"

# field NAME prints the value of the last answer's "NAME" string field, or nothing. Every body the
# probe reads is one line of JSON holding at most one such field.
field() {
  sed -n "s/.*\"$1\":\"\\([^\"]*\\)\".*/\\1/p" "$body_file" | sed -n 1p
}

# request BASE TARGET METHOD PATH HEADER_FILE [BODY] prints the HTTP status of a request that drew a
# whole answer, or 000 when it did not, and leaves the body in $body_file and curl's exit code in
# $exit_file. TARGET is an address to connect to in place of resolving the listener's host, or empty
# to resolve it as any client would.
request() {
  local base="$1" target="$2" method="$3" path="$4" header="$5" body="${6:-}" code exit_code=0
  local args=(-sS -o "$body_file" -w '%{http_code}' --connect-timeout 1 --max-time 3 -X "$method" -H "@${header}")
  [[ -z "$target" ]] || args+=(--resolve "${host}:${port}:${target}")
  [[ -z "$body" ]] || args+=(-H 'Content-Type: application/json' --data "$body")
  : >"$body_file"
  code="$(curl "${args[@]}" "${base}${path}" 2>/dev/null)" || exit_code=$?
  printf '%s\n' "$exit_code" >"$exit_file"
  ((exit_code == 0)) || code=000
  printf '%s' "$code"
}

# no_answer prints why the last request drew no whole answer, from curl's exit code: a name that
# did not resolve (6), nothing listening at the address (7), a timeout (28: the 1 s connect or the
# 3 s the probe allows a request), or a connection closed (52) or reset (56) without an answer,
# which is what Go's net/http does when a handler panics.
no_answer() {
  local exit_code
  exit_code="$(<"$exit_file")"
  case "$exit_code" in
    6) printf 'not resolved' ;;
    7) printf '%s' "$never_connected" ;;
    28) printf 'timeout' ;;
    52) printf 'empty reply' ;;
    56) printf 'connection reset' ;;
    *) printf 'curl exit %s' "$exit_code" ;;
  esac
}

# Per-target state, keyed by the target's label.
order=()
declare -A seen_ticks=() first_seen=() last_seen=() healthz_answered=() healthz_ok=() v1_ok=() \
  non200=() first_non200=() last_non200=() unanswered=() buckets=() send_outcomes=() dispatch_outcomes=()
v1_answers=0
v1_unauthorized=0

# count ARRAY KEY adds one to the associative ARRAY's count at KEY.
count() {
  local -n counts="$1"
  counts[$2]=$((${counts[$2]:-0} + 1))
}

record() {
  local target="$1" ts="$2" healthz_code="$3" v1_code="$4" v1_error="$5"
  if [[ -z "${seen_ticks[$target]+set}" ]]; then
    order+=("$target")
    first_seen[$target]="$ts"
  fi
  count seen_ticks "$target"
  last_seen[$target]="$ts"
  [[ "$healthz_code" == 000 ]] || count healthz_answered "$target"
  if [[ "$v1_code" != 000 ]]; then
    v1_answers=$((v1_answers + 1))
    [[ "$v1_code" != 401 && "$v1_code" != 403 ]] || v1_unauthorized=$((v1_unauthorized + 1))
  fi
  [[ "$v1_code" != 200 ]] || count v1_ok "$target"
  [[ "$healthz_code" == 200 ]] || return 0
  count healthz_ok "$target"
  [[ "$v1_code" != 200 ]] || return 0
  # /v1 found nothing listening right after /healthz answered: a listener that stops closes its
  # listening socket first, so the task stopped between the two requests. That is no answer from
  # the listener, so it is shown but is not a refusal. Any other request that drew no answer reached
  # a listener that failed it, and counts.
  if [[ "$v1_code" == 000 && "$v1_error" == "$never_connected" ]]; then
    count unanswered "$target"
    return 0
  fi
  count non200 "$target"
  [[ -n "${first_non200[$target]+set}" ]] || first_non200[$target]="$ts"
  last_non200[$target]="$ts"
  count buckets "${target}|${v1_code}:${v1_error}"
}

# post_dispatch TS posts one issue message targeting the Dispatch session and prints its delivery
# attempt's state, with the attempt's error after a colon when it has one, or http_<code> and the
# answer's error when Dispatch refused the message.
post_dispatch() {
  local ts="$1" code state attempt_error
  code="$(request "$dispatch_url" "" POST "/api/v1/issues/${dispatch_issue}/messages" "$dispatch_header" \
    "{\"body\":\"listener deploy probe ${ts}\",\"target\":\"session:${dispatch_session}\",\"delivery\":\"${dispatch_mode}\",\"actor\":{\"kind\":\"session\",\"id\":\"listener-deploy-probe\"}}")"
  if [[ "$code" != 201 ]]; then
    attempt_error="$(field error)"
    printf 'http_%s%s' "$code" "${attempt_error:+:${attempt_error}}"
    return
  fi
  # The answer is the message with its one delivery attempt.
  state="$(sed -n 's/.*"deliveries":\[[^]]*"state":"\([^"]*\)".*/\1/p' "$body_file")"
  attempt_error="$(sed -n 's/.*"deliveries":\[[^]]*"error":"\([^"]*\)".*/\1/p' "$body_file")"
  printf '%s%s' "${state:-unknown}" "${attempt_error:+:${attempt_error}}"
}

probe_tick() {
  local tick="$1" ts send_key="" dispatch_state="-" addresses=() target label healthz_code
  local healthz_status v1_code v1_error send_code dispatch_column
  printf -v ts '%(%Y-%m-%dT%H:%M:%SZ)T' -1
  if [[ ${#targets[@]} -gt 0 ]]; then
    addresses=("${targets[@]}")
  else
    mapfile -t addresses < <(getent ahostsv4 "$host" 2>/dev/null | sed -n 's/^\([0-9.]*\)[[:space:]][[:space:]]*STREAM.*/\1/p')
  fi
  [[ -z "$send_to" ]] || send_key="probe-${ts}"
  if ((dispatching)) && (((tick - 1) % dispatch_every == 0)); then
    dispatch_state="$(post_dispatch "$ts")"
    count dispatch_outcomes "$dispatch_state"
  fi
  for target in "${addresses[@]}" ""; do
    label="${target:-$host}"
    healthz_code="$(request "$url" "$target" GET /healthz "$envoy_header")"
    healthz_status="$(field status)"
    v1_code="$(request "$url" "$target" GET /v1/sessions "$envoy_header")"
    v1_error=""
    if [[ "$v1_code" == 000 ]]; then
      v1_error="$(no_answer)"
    elif [[ "$v1_code" != 200 ]]; then
      v1_error="$(field error)"
    fi
    send_code="-"
    if [[ -n "$send_to" ]]; then
      send_code="$(request "$url" "$target" POST /v1/messages/send "$envoy_header" \
        "{\"target_session\":\"${send_to}\",\"message\":\"deploy probe ${ts}\",\"idempotency_key\":\"${send_key}\"}")"
      count send_outcomes "$send_code"
    fi
    record "$label" "$ts" "$healthz_code" "$v1_code" "$v1_error"
    # The Dispatch message rides the name's line: Dispatch reaches the listener by its name.
    dispatch_column="-"
    [[ -n "$target" ]] || dispatch_column="$dispatch_state"
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$ts" "$label" "$healthz_code" "${healthz_status:--}" \
      "$v1_code" "${v1_error:--}" "$send_code" "$dispatch_column"
  done
}

finish() {
  local failing=0 reached=0 target bucket seen answered ok bad passed verdict why
  local posted=0 unsent=0 unsent_outcomes="" reasons=()
  printf '# summary\n'
  for target in "${order[@]}"; do
    seen="${seen_ticks[$target]}"
    answered="${healthz_answered[$target]:-0}"
    ok="${healthz_ok[$target]:-0}"
    bad="${non200[$target]:-0}"
    passed="${v1_ok[$target]:-0}"
    if ((answered == 0)); then
      printf 'target %s: unreached - /healthz never answered over %d ticks (%s to %s)\n' \
        "$target" "$seen" "${first_seen[$target]}" "${last_seen[$target]}"
      continue
    fi
    reached=$((reached + 1))
    verdict=pass why=""
    if ((bad > 0)); then
      verdict=fail why="; /v1 not 200 at a tick /healthz answered 200"
    elif ((passed == 0)); then
      verdict=fail why="; /v1 never answered 200"
    fi
    [[ "$verdict" == pass ]] || failing=$((failing + 1))
    printf 'target %s: %s - seen %s to %s; /healthz answered %d of %d ticks, 200 at %d; /v1 non-200 at %d of those%s\n' \
      "$target" "$verdict" "${first_seen[$target]}" "${last_seen[$target]}" "$answered" "$seen" "$ok" "$bad" "$why"
    if ((bad > 0)); then
      printf '  first non-200 %s, last %s\n' "${first_non200[$target]}" "${last_non200[$target]}"
      for bucket in "${!buckets[@]}"; do
        [[ "${bucket%%|*}" == "$target" ]] || continue
        printf '  %d x %s\n' "${buckets[$bucket]}" "${bucket#*|}"
      done
    fi
    if ((${unanswered[$target]:-0} > 0)); then
      printf '  %d tick(s) where /v1 found nothing listening right after /healthz answered 200 (the task stopped between the two requests; not a refusal)\n' \
        "${unanswered[$target]}"
    fi
  done
  if [[ -n "$send_to" ]]; then
    printf 'sends to %s:' "$send_to"
    for bucket in "${!send_outcomes[@]}"; do printf ' %s=%d' "$bucket" "${send_outcomes[$bucket]}"; done
    printf '\n'
  fi
  if ((dispatching)); then
    printf 'dispatch messages to %s:' "$dispatch_session"
    for bucket in "${!dispatch_outcomes[@]}"; do
      printf ' %s=%d' "$bucket" "${dispatch_outcomes[$bucket]}"
      posted=$((posted + ${dispatch_outcomes[$bucket]}))
      # An outcome is the attempt's state, or http_<code> for a post Dispatch refused, then any error
      # after a colon.
      [[ "${bucket%%:*}" != sent ]] || continue
      unsent=$((unsent + ${dispatch_outcomes[$bucket]}))
      unsent_outcomes+="${unsent_outcomes:+, }${bucket}=${dispatch_outcomes[$bucket]}"
    done
    printf '\n'
  fi
  if ((v1_answers > 0 && v1_unauthorized == v1_answers)); then
    printf 'verdict: misconfigured - every /v1 answer was 401 or 403; check the bearer\n'
    exit 2
  fi
  if ((reached == 0)); then
    printf 'verdict: fail - no target ever answered /healthz\n'
    exit 1
  fi
  ((failing == 0)) || reasons+=("${failing} target(s) refused /v1 while in service")
  ((unsent == 0)) || reasons+=("${unsent} of ${posted} Dispatch message(s) did not record state sent: ${unsent_outcomes}")
  if ((${#reasons[@]} > 0)); then
    verdict="${reasons[0]}"
    for why in "${reasons[@]:1}"; do verdict+="; ${why}"; done
    printf 'verdict: fail - %s\n' "$verdict"
    exit 1
  fi
  printf 'verdict: pass - %d target(s) served /v1 at every tick /healthz answered 200' "$reached"
  ((!dispatching)) || printf '; %d Dispatch message(s) recorded state sent' "$posted"
  printf '\n'
  exit 0
}

stop=0
trap 'stop=1' INT TERM
printf '# ts\ttarget\thealthz_code\thealthz_status\tv1_code\tv1_error\tsend_code\tdispatch_state\n'
ticks=0
((duration == 0)) || ticks=$(((duration + interval - 1) / interval))
tick=0
started=$SECONDS
while ((!stop)); do
  tick=$((tick + 1))
  probe_tick "$tick"
  ((ticks == 0 || tick < ticks)) || break
  next=$((started + tick * interval))
  ((SECONDS >= next)) || sleep $((next - SECONDS))
done
finish

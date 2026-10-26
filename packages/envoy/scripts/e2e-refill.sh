#!/usr/bin/env bash
# shellcheck disable=SC2317 # cleanup is trap-invoked
# e2e-refill.sh — Proves on the real listener and a real NATS server that the listener keeps its
# copy of a bucket while it reads the bucket again, and refuses its two list routes, with no
# active-sessions sample, while it cannot stand behind that copy (INFRA-261).
#
# Builds and starts a NATS server of its own (internal/testnats/cmd/natsd, its file store under the
# output directory) and the listener, as e2e-local.sh does, and drives them with refillrig
# (internal/testnats/cmd/refillrig): it seeds the session and interest buckets, through the
# listener's registration route and straight into the buckets, deletes and recreates a bucket,
# polls GET /v1/sessions, GET /v1/interests/ and /metrics as fast as they answer, and judges each
# scenario from those polls and the listener's own log lines. Each scenario prints one line:
#
#   verdict <scenario> PASS <what it saw>
#   verdict <scenario> FAIL <reason>
#   verdict <scenario> INVALID <reason>   the run missed the window it was meant to observe
#
# The listener's watcher finds a recreated bucket itself: it reads its bucket's stream every 2 s,
# and a new stream starts a read of the new bucket at once. The reread and interrupt scenarios stop
# the listener while the rig recreates and fills a bucket, so that read reads all of it, and time
# the recreate just after a tick of the listener's self-health monitor, which ticks every 30 s from
# the "envoy-listener ready" line, so no tick falls while the listener is stopped. recreate-live
# leaves the listener running and recreates 26 to 28 s before a tick, so the tick cannot be what
# reads the new bucket.
set -euo pipefail
# This rig's NATS is a throwaway server with no users. nats.go refuses an nkey when the server sends
# no nonce ("nats: nkeys not supported by the server"), so no process here inherits an operator's
# NATS_NKEY_SEED or NATS_NKEY_SEED_FILE.
unset NATS_NKEY_SEED NATS_NKEY_SEED_FILE

usage() {
  cat <<'EOF'
Usage: e2e-refill.sh [--scenario reread|interrupt|refuse|recreate-live|all]
                     [--interrupt-by stop|restart|both]
                     [--listener-bin <path>] [--natsd-bin <path>] [--out <dir>]
                     [--during-refuse <command>] [--after-refuse <command>]

Run the listener and a throwaway NATS server and prove, per scenario:
  reread         a recreated bucket's re-read keeps every record until it completes (session
                 bucket, interest bucket, and a recreate with an identical record set)
  interrupt      a re-read that NATS interrupts removes nothing and is read again by the listener
                 itself (stop: SIGSTOP past the 10 s idle timer; restart: kill and start NATS again)
  refuse         both lists answer the refilling 503 or a complete 200 during boot; the refilling
                 503, with no gauge sample, once NATS has been down past the 2-minute bound, then a
                 complete 200 once it is back; 200 [] and a 0 sample for empty buckets
  recreate-live  a session bucket recreated under a running listener, 26 to 28 s before a
                 self-health tick, is found and read by the watcher itself before that tick; no
                 list answer lacks a record of the old copy, or shows part of the new bucket while
                 the copy claims to follow it (an old stream shorter than the new one, and one
                 longer)

Options:
  --listener-bin, --natsd-bin  run these binaries instead of building them from this checkout,
                               e.g. a build of the merge base for a negative control
  --out <dir>                  evidence directory (default $TMPDIR/envoy-e2e-refill); its contents
                               are replaced
  --during-refuse <command>    run with bash -c while both lists refuse with NATS down, with
                               ENVOY_URL and the REFILL_RIG_* variables exported; its output goes
                               to <out>/refuse/during-refuse.log and NATS restarts once it exits
  --after-refuse <command>     the same, once both lists answer complete again
                               (<out>/refuse/after-refuse.log)

Exit status: 0 every verdict PASS, 1 a FAIL, 4 an INVALID and no FAIL, 2 usage, 3 environment.
Ports: E2E_REFILL_NATS_PORT (14232), E2E_REFILL_PORT (19030).
EOF
}

scenario=all
interrupt_by=both
listener_bin=""
natsd_bin=""
out_dir="${TMPDIR:-/tmp}/envoy-e2e-refill"
during_refuse=""
after_refuse=""
while (($#)); do
  case "$1" in
    --help | -h)
      usage
      exit 0
      ;;
    --scenario | --interrupt-by | --listener-bin | --natsd-bin | --out | --during-refuse | --after-refuse)
      if (($# < 2)); then
        printf 'ERR: %s needs a value\n' "$1" >&2
        usage >&2
        exit 2
      fi
      case "$1" in
        --scenario) scenario=$2 ;;
        --interrupt-by) interrupt_by=$2 ;;
        --listener-bin) listener_bin=$2 ;;
        --natsd-bin) natsd_bin=$2 ;;
        --out) out_dir=$2 ;;
        --during-refuse) during_refuse=$2 ;;
        --after-refuse) after_refuse=$2 ;;
      esac
      shift 2
      ;;
    *)
      printf 'ERR: unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done
case "$scenario" in
  reread | interrupt | refuse | recreate-live | all) ;;
  *)
    printf 'ERR: unknown scenario: %s\n' "$scenario" >&2
    exit 2
    ;;
esac
case "$interrupt_by" in
  stop | restart | both) ;;
  *)
    printf 'ERR: unknown --interrupt-by: %s\n' "$interrupt_by" >&2
    exit 2
    ;;
esac
for binary in "$listener_bin" "$natsd_bin"; do
  if [[ -n "$binary" && ! -x "$binary" ]]; then
    printf 'ERR: not an executable: %s\n' "$binary" >&2
    exit 2
  fi
done

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
envoy_dir="$(cd "${script_dir}/.." && pwd)"
readonly envoy_dir

nats_port="${E2E_REFILL_NATS_PORT:-14232}"
listener_port="${E2E_REFILL_PORT:-19030}"
readonly nats_port listener_port
readonly nats_url="nats://127.0.0.1:${nats_port}"
readonly listener_url="http://127.0.0.1:${listener_port}"

# Record counts. A re-read has to last long enough for polls to land inside it, and an interrupted
# one long enough for the interruption to land before its scan ends. A recreated bucket of the
# reread scenario also holds delete markers spread among its records: the read streams past them
# and no list answer carries them, so the read lasts many list answers.
readonly reread_count=3000
readonly reread_markers=100000
readonly interrupt_count=40000
readonly interrupt_side_count=1000
readonly boot_count=40000
# recreate-live: same and chg sessions of this many each in the old bucket, which is created with
# this MaxAge (the listener adopts a bucket's, so the renewed session's old record expires within a
# minute rather than five); that record expires this long after the recreate; NATS is stopped this
# long from the watcher's finding, well under the 10 s the listener's JetStream requests wait, so
# the read of the new bucket comes after that expiry; the longer shape writes same and chg this many
# times more.
readonly live_count=1000
readonly live_ttl_seconds=60
readonly live_renewed_expiry_ms=3500
readonly live_stop_seconds=4
readonly live_pads=2
readonly outage_count=300
readonly outage_api_count=3
# The listener's stale bound: 2 minutes, or half a bucket's MaxAge when that is shorter (the session
# bucket's is 5 minutes). The listener has no setting for it.
readonly stale_bound_seconds=120
readonly refill_role="refill-rig"
readonly refill_holder="ses_refill_api_0"

mkdir -p "$out_dir"
out_dir="$(cd "$out_dir" && pwd)"
readonly out_dir
readonly bin_dir="${out_dir}/bin"
readonly nats_store_dir="${out_dir}/nats-store"
readonly lock_file="${out_dir}/run.lock"
readonly rig="${bin_dir}/refillrig"

nats_pid=""
listener_pid=""
pollers=()
awaiter=""
awaiter_out=""
awaited=""
run_dir=""
listener_log=""
nats_starts=0
ready_ns=0
listener_start_offset=0
verdicts=()
phase_result=""

say() { printf 'e2e-refill: %s\n' "$*" >&2; }

# cleanup stops what this run started and exits with rc, the status the trap that ran it passes (see
# e2e-local.sh): a stopped NATS is continued first, since it takes no TERM while stopped.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  set +e
  for pid in "${pollers[@]}" "$awaiter" "$listener_pid" "$nats_pid"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -CONT "$pid" 2>/dev/null
      kill "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null
    fi
  done
  rm -rf "$nats_store_dir"
  exit "$rc"
}

for command in curl flock go jq setsid; do
  command -v "$command" >/dev/null || {
    printf 'ERR: required command is unavailable: %s\n' "$command" >&2
    exit 3
  }
done

# The output directory belongs to one run at a time, as e2e-local.sh's does.
exec {lock_fd}>"$lock_file"
flock -n "$lock_fd" || {
  printf 'ERR: another run of e2e-refill.sh is using %s; this run removed nothing.\n' "$out_dir" >&2
  exit 3
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

rm -rf "$bin_dir" "$nats_store_dir" "${out_dir}/reread" "${out_dir}/interrupt-stop" \
  "${out_dir}/interrupt-restart" "${out_dir}/refuse" "${out_dir}/recreate-live"
mkdir -p "$bin_dir"

say "building refillrig, and the listener and natsd unless given"
(
  cd "$envoy_dir"
  go build -o "$rig" ./internal/testnats/cmd/refillrig
  [[ -n "$listener_bin" ]] || go build -o "${bin_dir}/listener" ./cmd/listener
  [[ -n "$natsd_bin" ]] || go build -o "${bin_dir}/natsd" ./internal/testnats/cmd/natsd
)
listener_binary="${listener_bin:-${bin_dir}/listener}"
natsd_binary="${natsd_bin:-${bin_dir}/natsd}"
readonly listener_binary natsd_binary
say "listener: ${listener_binary}"
say "natsd: ${natsd_binary}"

# start_nats starts NATS on the rig's port and store, each start with a log of its own, and returns
# once this server, not another one holding the port, answers (e2e-local.sh's wait_for_nats). NATS
# runs in a session of its own (setsid): a stopped process in this script's own process group can
# have the whole group hung up (an orphaned group with a stopped member gets SIGHUP), which would
# end this script the moment the interrupt scenario stops NATS.
start_nats() {
  nats_starts=$((nats_starts + 1))
  local nats_log="${run_dir}/nats-${nats_starts}.log"
  : >"$nats_log"
  setsid "$natsd_binary" -p "$nats_port" -store_dir "$nats_store_dir" >"$nats_log" 2>&1 &
  nats_pid=$!
  local deadline=$((SECONDS + 60)) banner
  while ((SECONDS < deadline)); do
    if ! kill -0 "$nats_pid" 2>/dev/null; then
      say "ERR: the NATS server this script started ended before it was ready"
      cat "$nats_log" >&2
      return 1
    fi
    if grep -q 'Server is ready' "$nats_log"; then
      banner="$(curl -s --max-time 1 "telnet://127.0.0.1:${nats_port}" </dev/null 2>/dev/null || true)"
      if [[ "$banner" == 'INFO '* ]]; then
        return 0
      fi
    fi
    sleep 0.25
  done
  say "ERR: NATS did not accept connections on 127.0.0.1:${nats_port} within 60 seconds"
  return 1
}

# stop_nats ends NATS with SIGTERM, which closes its clients' connections, continuing it first if
# it was stopped.
stop_nats() {
  if [[ -n "$nats_pid" ]] && kill -0 "$nats_pid" 2>/dev/null; then
    kill -CONT "$nats_pid" 2>/dev/null || true
    kill "$nats_pid" 2>/dev/null || true
    wait "$nats_pid" 2>/dev/null || true
  fi
  nats_pid=""
}

# start_listener starts the listener on the rig's NATS. Every start of a scenario appends to the
# scenario's one listener.log, so it records where its own lines begin (listener_start_offset). It
# runs in a session of its own, as NATS does (start_nats), since the reread and interrupt scenarios
# stop it.
start_listener() {
  listener_log="${run_dir}/listener.log"
  touch "$listener_log"
  listener_start_offset="$(stat -c %s "$listener_log")"
  PORT="$listener_port" \
    ENVOY_MACHINE_ID=e2e-refill \
    NATS_URLS="$nats_url" \
    setsid "$listener_binary" >>"$listener_log" 2>&1 &
  listener_pid=$!
}

# await_line waits for the listener log line the arguments name (refillrig await) and prints its
# time, the time of the signal it sent (0 for none), and the offset past it.
await_line() {
  "$rig" await -log "$listener_log" "$@"
}

# await_start runs refillrig await in the background into file, for a line due while this shell is
# busy: a recreate it runs, or a listener it is about to continue, whose line must be acted on the
# moment it appears. await_finish waits for it, sets awaited to what it printed, and fails when it
# found no line. Both run in this shell, never a command substitution, which could not wait for it.
await_start() {
  awaiter_out=$1
  shift
  "$rig" await -log "$listener_log" "$@" >"$awaiter_out" &
  awaiter=$!
}

await_finish() {
  local rc=0
  wait "$awaiter" 2>/dev/null || rc=$?
  awaiter=""
  awaited="$(cat "$awaiter_out")"
  return "$rc"
}

# wait_ready waits for this start's "envoy-listener ready" line, from whose time the listener's
# self-health monitor ticks every 30 s (align_tick).
wait_ready() {
  local line
  line="$(await_line -offset "$listener_start_offset" -match 'envoy-listener ready' -timeout 180s)"
  ready_ns="${line%% *}"
}

# stop_listener ends the listener with SIGTERM, and with SIGKILL when its ordered shutdown has not
# ended it within 40 seconds (its own budget is 23), continuing it first if it was stopped.
stop_listener() {
  if [[ -n "$listener_pid" ]] && kill -0 "$listener_pid" 2>/dev/null; then
    kill -CONT "$listener_pid" 2>/dev/null || true
    kill "$listener_pid" 2>/dev/null || true
    local waited=0
    while kill -0 "$listener_pid" 2>/dev/null && ((waited < 160)); do
      sleep 0.25
      waited=$((waited + 1))
    done
    kill -KILL "$listener_pid" 2>/dev/null || true
    wait "$listener_pid" 2>/dev/null || true
  fi
  listener_pid=""
}

# fresh stops whatever runs, empties the store, and starts NATS for the scenario directory name.
fresh() {
  stop_listener
  stop_nats
  rm -rf "$nats_store_dir"
  run_dir="${out_dir}/$1"
  mkdir -p "$run_dir"
  nats_starts=0
  start_nats
}

# poll_start polls one list (sessions, interests or metrics) into <run_dir>/<name>-<list>.jsonl
# until poll_stop.
poll_start() {
  local name=$1 list=$2 sets=$3
  shift 3
  "$rig" poll -url "$listener_url" -list "$list" -sets "$sets" -out "${run_dir}/${name}-${list}.jsonl" "$@" &
  pollers+=("$!")
}

poll_stop() {
  local pid
  for pid in "${pollers[@]}"; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  pollers=()
}

# wait_whole waits until the list (sessions or interests) answers 200 with count rows.
wait_whole() {
  local list=$1 count=$2 path deadline=$((SECONDS + ${3:-120})) length
  path="/v1/sessions"
  [[ "$list" == interests ]] && path="/v1/interests/"
  while ((SECONDS < deadline)); do
    if length="$(curl -fsS --max-time 30 "${listener_url}${path}" 2>/dev/null | jq length 2>/dev/null)" &&
      [[ "$length" == "$count" ]]; then
      return 0
    fi
    sleep 0.5
  done
  say "ERR: GET ${path} did not answer ${count} rows within ${3:-120} seconds (last: ${length:-no answer})"
  return 1
}

# align_tick returns 1.5 s after the listener's next self-health tick, so a recreate that follows,
# and the seconds the listener is stopped for it, end long before the tick after it.
align_tick() {
  local now_ms phase
  now_ms=$(($(date +%s%N) / 1000000))
  phase=$(((now_ms - ready_ns / 1000000) % 30000))
  sleep_until_ms $((now_ms + (31500 - phase) % 30000))
}

# sleep_until_ms sleeps until the Unix time in milliseconds, if it is still ahead.
sleep_until_ms() {
  local wait_ms=$(($1 - $(date +%s%N) / 1000000))
  ((wait_ms > 0)) || return 0
  sleep "$((wait_ms / 1000)).$(printf '%03d' $((wait_ms % 1000)))"
}

log_offset() {
  stat -c %s "$listener_log"
}

# verdict prints the scenario's verdict line and records it for the exit status.
verdict() {
  local name=$1 line=$2
  printf 'verdict %s %s\n' "$name" "$line"
  verdicts+=("${line%% *}")
}

# combine folds the phase verdict lines given as "phase: line" into one: FAIL naming every phase
# that failed, else INVALID naming every phase that missed its window, else PASS with every
# phase's notes; phases are separated by " | ".
combine() {
  local entry phase line failed=() invalid=() passed=()
  for entry in "$@"; do
    phase="${entry%%: *}"
    line="${entry#*: }"
    case "$line" in
      FAIL*) failed+=("${phase}: ${line#FAIL }") ;;
      INVALID*) invalid+=("${phase}: ${line#INVALID }") ;;
      *) passed+=("${phase}: ${line#PASS }") ;;
    esac
  done
  if ((${#failed[@]})); then
    printf 'FAIL %s\n' "$(join_phases "${failed[@]}")"
  elif ((${#invalid[@]})); then
    printf 'INVALID %s\n' "$(join_phases "${invalid[@]}")"
  else
    printf 'PASS %s\n' "$(join_phases "${passed[@]}")"
  fi
}

join_phases() {
  local first=$1
  shift
  printf '%s' "$first" "${@/#/ | }"
}

# judge runs refillrig judge, turning a rig error into an INVALID line, never a pass. Each command
# is recorded in <run_dir>/judge.commands, so the evidence can be judged again.
judge() {
  printf '%q ' "$rig" judge "$@" >>"${run_dir}/judge.commands"
  printf '\n' >>"${run_dir}/judge.commands"
  "$rig" judge "$@" 2>"${run_dir}/judge.err" || printf 'INVALID the rig could not judge: %s\n' "$(tr '\n' ' ' <"${run_dir}/judge.err")"
}

# reread_phase recreates one bucket just after a self-health tick, polls both lists until the read
# of the new bucket has completed, and judges it into phase_result. The listener is stopped while
# the rig deletes, creates and fills the bucket, so the read its watcher begins once it runs again
# (its stream probe finds the new stream at once) reads the whole bucket, as one that a recreate
# between two probes gets. It runs in this shell, not a command substitution, so the pollers it
# starts are this shell's to stop on any exit.
reread_phase() {
  local name=$1 kind=$2 before=$3 after=$4 steady=$5 universe=$6
  local registry="session registry" steady_list=interests offset begin
  if [[ "$kind" == interests ]]; then
    registry="interest registry"
    steady_list=sessions
  fi
  offset="$(log_offset)"
  poll_start "$name" sessions "$universe"
  poll_start "$name" interests "$universe"
  align_tick
  say "${name}: recreating the ${kind} bucket with ${after} and ${reread_markers} delete markers, the listener stopped"
  kill -STOP "$listener_pid"
  if ! "$rig" recreate -nats "$nats_url" -bucket "$kind" -sets "$after" -markers "$reread_markers" >&2; then
    kill -CONT "$listener_pid"
    poll_stop
    phase_result="INVALID the rig could not recreate the ${kind} bucket"
    return 0
  fi
  kill -CONT "$listener_pid"
  if begin="$(await_line -offset "$offset" -match "${registry} bucket was recreated;" -timeout 75s)"; then
    await_line -offset "$offset" -match "${registry} cache warm-up@completed" \
      -after-ns "${begin%% *}" -timeout 90s >/dev/null || true
  fi
  sleep 3
  poll_stop
  phase_result="$(judge reread -log "$listener_log" -offset "$offset" -registry "$registry" \
    -obs "${run_dir}/${name}-${kind}.jsonl" -before "$before" -after "$after" \
    -steady-obs "${run_dir}/${name}-${steady_list}.jsonl" -steady-sets "$steady")"
}

# run_reread registers old and both through the listener, then recreates the session bucket
# holding both with changed titles and new, the interest bucket holding both with changed topics
# and new, and the session bucket again with the very records it holds, so only its creation time
# changes. A phase that could not recreate its bucket ends the scenario: what the next phase
# expects the buckets to hold is unknown.
run_reread() {
  local n=$reread_count phases=() universe first second phase label name kind before after steady
  universe="old=${n},both=${n},new=${n}"
  first="old=${n}:1,both=${n}:1"
  second="both=${n}:2,new=${n}:2"
  fresh reread
  start_listener
  wait_ready
  say "reread: registering old and both, ${n} sessions each, through the listener"
  "$rig" register -url "$listener_url" -sets "old=${n},both=${n}" >&2
  wait_whole sessions $((2 * n))
  wait_whole interests $((2 * n))
  # Each phase: label|poll file name|bucket|the copy before|the bucket after|the other list's records.
  for phase in "session bucket|sessions|sessions|${first}|${second}|${first}" \
    "interest bucket|interests|interests|${first}|${second}|${second}" \
    "identical records|identical|sessions|${second}|${second}|${second}"; do
    IFS='|' read -r label name kind before after steady <<<"$phase"
    reread_phase "$name" "$kind" "$before" "$after" "$steady" "$universe"
    phases+=("${label}: ${phase_result}")
    say "${phases[-1]}"
    [[ "$phase_result" != "INVALID the rig could not recreate"* ]] || break
  done
  verdict reread "$(combine "${phases[@]}")"
}

# run_interrupt interrupts the read of a recreated 41,000-record session bucket the moment it
# begins: the listener is stopped while the rig recreates and fills the bucket, and the read its
# watcher begins once it runs again is the one interrupted, by SIGSTOP (mode stop) or SIGKILL and a
# restart (mode restart) of NATS, sent the moment that read's install line appears.
run_interrupt() {
  local mode=$1 n=$interrupt_count side=$interrupt_side_count offset line interrupted signal=STOP
  fresh "interrupt-${mode}"
  start_listener
  wait_ready
  say "interrupt (${mode}): seeding old=${side} and both=${n} sessions, and their interests"
  "$rig" seed -nats "$nats_url" -buckets sessions -sets "old=${side},both=${n}" >&2
  # Every session has an interest, so no list row needs a bucket read to join one.
  "$rig" seed -nats "$nats_url" -buckets interests -sets "old=${side},both=${n},new=${side}" >&2
  wait_whole sessions $((n + side))
  wait_whole interests $((n + 2 * side))
  offset="$(log_offset)"
  poll_start interrupt sessions "old=${side},both=${n},new=${side}" -interval 200ms
  align_tick
  say "interrupt (${mode}): recreating the session bucket with both=${n}:2,new=${side}:2, the listener stopped"
  kill -STOP "$listener_pid"
  if ! "$rig" recreate -nats "$nats_url" -bucket sessions -sets "both=${n}:2,new=${side}:2" >&2; then
    kill -CONT "$listener_pid"
    poll_stop
    verdict "interrupt(${mode})" "INVALID the rig could not recreate the session bucket"
    return 0
  fi
  [[ "$mode" == stop ]] || signal=KILL
  await_start "${run_dir}/read.await" -offset "$offset" -match 'session registry bucket was recreated;' \
    -timeout 75s -signal "$signal" -pid "$nats_pid"
  sleep 0.2
  kill -CONT "$listener_pid"
  if await_finish; then
    line=$awaited
  else
    line="0 0 0"
  fi
  interrupted="$(cut -d' ' -f2 <<<"$line")"
  case "$mode" in
    stop)
      # Stopped, NATS sends nothing more, and once the listener has applied what it already
      # received, nats.go's 10 s idle timer ends the scan as timed out (or the watcher ends). NATS is
      # continued only once the listener's log says which, or after a minute.
      say "interrupt (stop): NATS stopped; continuing it once the scan times out or the watcher ends"
      await_line -offset "$offset" -after-ns "$interrupted" -timeout 60s \
        -match 'session registry cache warm-up@timed out' -match 'session registry watcher stopped' >/dev/null || true
      kill -CONT "$nats_pid"
      say "interrupt (stop): NATS continued $((($(date +%s%N) - interrupted) / 1000000)) ms after it was stopped"
      ;;
    restart)
      # A run whose read never began killed nothing: kill NATS here, so the wait cannot hang.
      kill -KILL "$nats_pid" 2>/dev/null || true
      wait "$nats_pid" 2>/dev/null || true
      nats_pid=""
      say "interrupt (restart): NATS killed; starting it again in 2 s"
      sleep 2
      start_nats
      ;;
  esac
  await_line -offset "$offset" -match 'session registry cache warm-up@completed' \
    -after-ns "$interrupted" -timeout 120s >/dev/null || true
  sleep 10
  poll_stop
  verdict "interrupt(${mode})" "$(judge interrupt -log "$listener_log" -offset "$offset" \
    -obs "${run_dir}/interrupt-sessions.jsonl" -before "old=${side}:1,both=${n}:1" \
    -after "both=${n}:2,new=${side}:2" -interrupted-ns "$interrupted" -mode "$mode")"
}

# live_shape runs one shape of recreate-live into phase_result, on a NATS and listener of its own.
# The old session bucket has a 60 s MaxAge and holds hb, written so its old record expires 3.5 s
# after the recreate, and same and chg, written pads+1 times, which makes the old stream shorter (0)
# or longer (2) than the new one. The recreate lands 2 to 4 s after a self-health tick, 26 to 28 s
# before the next, with the listener running, and 1 s after one of the session watcher's stream
# probes, which it sends every 2 s from its first read: the fill, some 100 ms, is done before the
# next probe finds the new stream. The new bucket holds new, chg at a changed value, same as it was
# and hb renewed. NATS is stopped for 4 s from the moment the watcher logs that finding, which holds
# the read of the new bucket back, so hb's old record expires after the copy stopped following and
# before the read delivers its renewal: the copy must keep it. A listener that finds a recreate only
# by the tick's Check fails both shapes.
live_shape() {
  local shape=$1 pads=$2 n=$live_count pad offset line now_ms ready_ms probe_ms recreate_ms
  local renewed_from renewed_to
  local before="same=${n}:1,chg=${n}:1,hb=1:1" after="new=${n}:2,chg=${n}:2,same=${n}:1,hb=1:2"
  local universe="same=${n},chg=${n},new=${n},hb=1"
  fresh "recreate-live/${shape}"
  "$rig" seed -nats "$nats_url" -create -buckets sessions -ttl "${live_ttl_seconds}s" -sets "" >&2
  "$rig" seed -nats "$nats_url" -create -buckets interests -sets "$universe" >&2
  start_listener
  wait_ready
  # The session bucket is empty, so its first read's warm-up line comes as the read begins, and
  # with it the watcher's first stream probe is armed.
  line="$(await_line -offset "$listener_start_offset" -match 'session registry cache warm-up' -timeout 10s)"
  probe_ms=$((${line%% *} / 1000000))
  now_ms=$(($(date +%s%N) / 1000000))
  ready_ms=$((ready_ns / 1000000))
  recreate_ms=$((ready_ms + ((now_ms + 56000 - ready_ms) / 30000 + 1) * 30000 + 2000 + (3000 - (ready_ms - probe_ms) % 2000) % 2000))
  say "recreate-live (${shape}): recreating the session bucket in $(((recreate_ms - now_ms) / 1000)) s, $(((recreate_ms - ready_ms) % 30000)) ms after a self-health tick"
  sleep_until_ms $((recreate_ms + live_renewed_expiry_ms - live_ttl_seconds * 1000))
  renewed_from="$(date +%s%N)"
  "$rig" seed -nats "$nats_url" -buckets sessions -sets "hb=1:1" >&2
  renewed_to="$(date +%s%N)"
  # same and chg are written late enough that their old records outlive the shape, even for a
  # listener whose copy follows the deleted bucket until the tick's read.
  sleep_until_ms $((recreate_ms - 22000))
  for ((pad = 0; pad <= pads; pad++)); do
    "$rig" seed -nats "$nats_url" -buckets sessions -sets "same=${n}:1,chg=${n}:1" >&2
  done
  wait_whole sessions $((2 * n + 1)) 15
  offset="$(log_offset)"
  poll_start live sessions "$universe"
  await_start "${run_dir}/found.await" -offset "$offset" -match 'session registry bucket was recreated under its watcher' \
    -timeout 40s -signal STOP -pid "$nats_pid" -cont-after "${live_stop_seconds}s"
  sleep_until_ms "$recreate_ms"
  say "recreate-live (${shape}): recreating the session bucket with ${after}, the listener running"
  if ! "$rig" recreate -nats "$nats_url" -bucket sessions -sets "$after" -fill-log "${run_dir}/fill.json" >&2; then
    kill "$awaiter" 2>/dev/null || true
    await_finish || true
    kill -CONT "$nats_pid" 2>/dev/null || true
    poll_stop
    phase_result="INVALID the rig could not recreate the session bucket"
    return 0
  fi
  await_finish || say "recreate-live (${shape}): the watcher never logged that it found the recreate; NATS was not stopped"
  await_line -offset "$offset" -match 'session registry cache warm-up@completed' \
    -after-ns "$((recreate_ms * 1000000))" -timeout 60s >/dev/null || true
  sleep 3
  poll_stop
  phase_result="$(judge recreate-live -log "$listener_log" -offset "$offset" \
    -obs "${run_dir}/live-sessions.jsonl" -before "$before" -after "$after" \
    -fill-log "${run_dir}/fill.json" -ready-ns "$ready_ns" -renewed hb \
    -renewed-from-ns "$renewed_from" -renewed-to-ns "$renewed_to" -ttl "${live_ttl_seconds}s" -shape "$shape")"
}

run_recreate_live() {
  local phases=()
  live_shape shorter 0
  phases+=("shorter old stream: ${phase_result}")
  say "${phases[-1]}"
  live_shape longer "$live_pads"
  phases+=("longer old stream: ${phase_result}")
  say "${phases[-1]}"
  verdict recreate-live "$(combine "${phases[@]}")"
}

# run_hook runs a --during-refuse or --after-refuse command with the listener's URL and the seeded
# role's holder exported, its output in <run_dir>/<name>.log.
run_hook() {
  local name=$1 command=$2 rc=0
  [[ -n "$command" ]] || return 0
  say "running --${name}: ${command}"
  ENVOY_URL="$listener_url" \
    REFILL_RIG_ROLE="$refill_role" \
    REFILL_RIG_HOLDER="$refill_holder" \
    REFILL_RIG_SESSIONS=$((outage_count + outage_api_count)) \
    REFILL_RIG_DIR="$run_dir" \
    bash -c "$command" >"${run_dir}/${name}.log" 2>&1 || rc=$?
  say "--${name} exited ${rc}; its output is ${run_dir}/${name}.log"
  sed 's/^/  | /' "${run_dir}/${name}.log" >&2
}

# list_answer prints what a list route answers right now: its status and body, cut short.
list_answer() {
  curl -sS --max-time 10 -D - "${listener_url}$1" 2>&1 | tr -d '\r' | grep -E '^(HTTP|Retry-After)|^[[{]' | cut -c1-200 | tr '\n' ' '
}

run_refuse() {
  local parts=() sets down_ns restart_ns seeded_ns status_sessions status_interests body metrics_text
  local total=$((outage_count + outage_api_count))

  # (a) A start on full buckets: every list answer is the refilling 503 or the whole bucket.
  fresh refuse
  say "refuse (boot): seeding ${boot_count} sessions and interests before the listener starts"
  "$rig" seed -nats "$nats_url" -create -buckets sessions,interests -sets "boot=${boot_count}" >&2
  poll_start boot sessions "boot=${boot_count}"
  poll_start boot interests "boot=${boot_count}"
  poll_start boot metrics "" -interval 20ms
  start_listener
  wait_ready
  wait_whole sessions "$boot_count"
  wait_whole interests "$boot_count"
  sleep 2
  poll_stop
  parts+=("boot: $(judge boot -sessions-obs "${run_dir}/boot-sessions.jsonl" \
    -interests-obs "${run_dir}/boot-interests.jsonl" -metrics-obs "${run_dir}/boot-metrics.jsonl" \
    -sets "boot=${boot_count}")")
  say "${parts[-1]}"

  # (b) NATS down past the stale bound, then back.
  stop_listener
  stop_nats
  rm -rf "$nats_store_dir"
  start_nats
  start_listener
  wait_ready
  sets="bulk=${outage_count},api=${outage_api_count}"
  seeded_ns="$(date +%s%N)"
  "$rig" seed -nats "$nats_url" -buckets sessions,interests -sets "bulk=${outage_count}" >&2
  "$rig" register -url "$listener_url" -sets "api=${outage_api_count}" -role "$refill_role" >&2
  wait_whole sessions "$total"
  wait_whole interests "$total"
  # An outage lasts minutes and the hooks run inside it, so these polls leave the CPU some room.
  poll_start outage sessions "$sets" -interval 100ms
  poll_start outage interests "$sets" -interval 100ms
  poll_start outage metrics "" -interval 250ms
  sleep 3
  down_ns="$(date +%s%N)"
  say "refuse (outage): NATS stopped; waiting $((stale_bound_seconds + 10)) s, past the ${stale_bound_seconds} s bound"
  stop_nats
  sleep "$((stale_bound_seconds + 10))"
  say "refuse (outage): GET /v1/sessions answers: $(list_answer /v1/sessions)"
  say "refuse (outage): GET /v1/interests/ answers: $(list_answer /v1/interests/)"
  run_hook during-refuse "$during_refuse"
  restart_ns="$(date +%s%N)"
  start_nats
  say "refuse (outage): NATS started again"
  wait_whole sessions "$total" 60 || true
  wait_whole interests "$total" 60 || true
  sleep 3
  run_hook after-refuse "$after_refuse"
  poll_stop
  parts+=("outage: $(judge outage -sessions-obs "${run_dir}/outage-sessions.jsonl" \
    -interests-obs "${run_dir}/outage-interests.jsonl" -metrics-obs "${run_dir}/outage-metrics.jsonl" \
    -sets "$sets" -down-ns "$down_ns" -restart-ns "$restart_ns" -seeded-ns "$seeded_ns" \
    -bound "${stale_bound_seconds}s")")
  say "${parts[-1]}"

  # (c) Empty buckets: a complete read of nothing is 200 [] and a 0 sample.
  stop_listener
  stop_nats
  rm -rf "$nats_store_dir"
  start_nats
  start_listener
  wait_ready
  if ! wait_whole sessions 0 60 || ! wait_whole interests 0 60; then
    parts+=("empty: FAIL the lists of empty buckets never answered 200 []: GET /v1/sessions $(list_answer /v1/sessions)")
  else
    status_sessions="$(curl -sS -o "${run_dir}/empty-sessions.json" -w '%{http_code}' "${listener_url}/v1/sessions")"
    status_interests="$(curl -sS -o "${run_dir}/empty-interests.json" -w '%{http_code}' "${listener_url}/v1/interests/")"
    metrics_text="$(curl -sS "${listener_url}/metrics" | grep -E '^envoy_active_(sessions|interests) ' | tr '\n' ' ')"
    body="$(cat "${run_dir}/empty-sessions.json") $(cat "${run_dir}/empty-interests.json")"
    if [[ "$status_sessions $status_interests" == "200 200" && "$body" == "[] []" &&
      "$metrics_text" == *"envoy_active_sessions 0 "* && "$metrics_text" == *"envoy_active_interests 0 "* ]]; then
      parts+=("empty: PASS both lists 200 [], ${metrics_text% }")
    else
      parts+=("empty: FAIL lists ${status_sessions} ${status_interests} ${body}, metrics: ${metrics_text:-no samples}")
    fi
  fi
  say "${parts[-1]}"
  verdict refuse "$(combine "${parts[@]}")"
}

case "$scenario" in
  reread | all) run_reread ;;&
  interrupt | all)
    [[ "$interrupt_by" == restart ]] || run_interrupt stop
    [[ "$interrupt_by" == stop ]] || run_interrupt restart
    ;;&
  recreate-live | all) run_recreate_live ;;&
  refuse | all) run_refuse ;;
esac

stop_listener
stop_nats
status=0
for result in "${verdicts[@]}"; do
  case "$result" in
    FAIL) status=1 ;;
    INVALID) ((status == 1)) || status=4 ;;
  esac
done
say "evidence: ${out_dir}"
exit "$status"

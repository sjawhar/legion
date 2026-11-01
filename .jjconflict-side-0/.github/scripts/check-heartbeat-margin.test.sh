#!/usr/bin/env bash
# Tests for `check-heartbeat-margin.sh`: the interval a pane refreshes its Envoy registration at
# leaves a margin inside the lifetime the listener keeps a session record for, both of the Legion
# daemon's copies of that interval agree with it - `Heartbeat`, whose staleness window must cover
# that lifetime, and `pluginHeartbeat`, which is `noticeReholdDelays[1]` - and the check refuses
# any tree it cannot read unambiguously. The failure it stops is a silent one — a live pane's
# record expires and every other agent is told there is no live session for it — so the suite pins
# the exit status and the wording the author of a drift has to act on, including that the third
# refresh must complete strictly inside the lifetime rather than merely be attempted - a renewal
# window equal to the lifetime is refused like one past it, and the remedies the refusal names
# pass when applied - and that a pass-deadline floor far above the heartbeat is not refused.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real script
# into it, so the script under test is the file CI runs. The fixtures' numbers are invented and are
# never the repository's real ones: a hardcoded copy of any real number in the check would fail
# this suite.
#
# Run from anywhere: .github/scripts/check-heartbeat-margin.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-heartbeat-margin.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name> [heartbeat-ms] [pass-deadline-floor-ms] [timeout-ms] [retry-delay-ms] [attempts]
#         [record-lifetime] [daemon-heartbeat] [window-heartbeats] [plugin-heartbeat]
#         [listener-session-ttl] [claim-stale-after] [listener-claim-stale]: a tree that
# passes — three refresh intervals plus one listener call strictly inside the record lifetime,
# both of the daemon's copies of the interval equal to the TypeScript one, its window covering
# that lifetime, and its two copies of the listener's liveness windows equal to the windows
# themselves. The pass-deadline floor is written because the real file declares one; the check
# must not read it, and the assertion that it is quoted nowhere is only a test of that while this
# line keeps being written.
# Echoes its root.
fixture() {
  local root="$work/$1" heartbeat=${2:-40_000} deadline=${3:-12_000} timeout=${4:-2_000}
  local retry=${5:-400} attempts=${6:-2} lifetime=${7:-3 * time.Minute}
  local daemon_heartbeat=${8:-40 * time.Second} window=${9:-5} plugin=${10:-40 * time.Second}
  local claim_stale=${12:-4 * time.Minute}
  local listener_ttl=${11:-$lifetime} listener_claim_stale=${13:-$claim_stale}
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/envoy-client/src" \
    "$root/packages/envoy/internal/session" "$root/packages/daemon/internal/controller" \
    "$root/packages/daemon/internal/daemon"
  ln -s "$check_script" "$root/.github/scripts/check-heartbeat-margin.sh"
  cat > "$root/packages/envoy-client/src/envoy-settings.ts" <<TS
export const DEFAULT_ENVOY_URL = "http://127.0.0.1:9020";
// The listener expires a session record after OpenSessionRegistry's default ttl
// (packages/envoy/internal/session/registry.go), and a record is renewed at a third of that.
export const DEFAULT_HEARTBEAT_MS = $heartbeat;
/** The floor under a pass deadline when nothing names one and the heartbeat is shorter than it. */
export const MIN_PASS_DEADLINE_MS = $deadline;
TS
  cat > "$root/packages/envoy-client/src/transport.ts" <<TS
const DEFAULT_TIMEOUT_MS = $timeout;
const RETRY_DELAY_MS = $retry;

export function createEnvoyClient(config: EnvoyClientConfig): EnvoyClient {
  const request = async (path: string, init: RequestInit): Promise<string> => {
    const attempts = config.retry === false ? 1 : $attempts;
    return String(attempts);
  };
  return { request };
}
TS
  cat > "$root/packages/envoy/internal/session/registry.go" <<GO
package session

func OpenSessionRegistry(conn *nats.Conn, options ...SessionRegistryOption) (*SessionRegistry, error) {
    opts := sessionRegistryOpts{replicas: 1, ttl: $lifetime}
    return newSessionRegistry(opts)
}
GO
  cat > "$root/packages/daemon/internal/controller/liveness.go" <<GO
package controller

// Heartbeat is the pane's registration heartbeat (DEFAULT_HEARTBEAT_MS,
// packages/envoy-client/src/envoy-settings.ts).
const Heartbeat = $daemon_heartbeat

func LivenessWindow(bootTimeout time.Duration) time.Duration {
    return max(bootTimeout, $window*Heartbeat)
}
GO
  cat > "$root/packages/daemon/internal/daemon/notice_exceptions.go" <<GO
package daemon

// The plugin heartbeats every pluginHeartbeat (DEFAULT_HEARTBEAT_MS,
// packages/envoy-client/src/envoy-settings.ts).
const (
    listenerSessionTTL = $listener_ttl
    pluginHeartbeat    = $plugin
    listenerClaimStale = $listener_claim_stale
)

var noticeReholdDelays = [...]time.Duration{20 * time.Second, pluginHeartbeat, max(listenerSessionTTL, listenerClaimStale) + time.Minute}
GO
  cat > "$root/packages/envoy/internal/session/claim.go" <<GO
package session

// ClaimStaleAfter is how long a claim may go unrefreshed before another holder may take the
// route.
const ClaimStaleAfter = $claim_stale
GO
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$("$root/.github/scripts/check-heartbeat-margin.sh" 2>&1)
  status=$?
  set -e
}

echo "case: a tree with a margin everywhere"
root=$(fixture green)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "names the interval, the call budget and their sites" \
  "$(contains "$out" '^3 refresh intervals of 40000 ms (packages/envoy-client/src/envoy-settings.ts:4) plus one listener call 4400 ms (2 attempts of 2000 ms, packages/envoy-client/src/transport.ts:1, 400 ms apart, packages/envoy-client/src/transport.ts:2; attempts at packages/envoy-client/src/transport.ts:6) are 124400 ms')"
check "names the record lifetime and its site" \
  "$(contains "$out" 'are 124400 ms, strictly inside the session record lifetime 180000 ms (packages/envoy/internal/session/registry.go:4)')"
check "names the daemon's heartbeat, window and second copy with their sites" \
  "$(contains "$out" "daemon's Heartbeat 40000 ms (packages/daemon/internal/controller/liveness.go:5) matches and its window 5 x Heartbeat = 200000 ms (packages/daemon/internal/controller/liveness.go:8) covers that lifetime; its pluginHeartbeat 40000 ms (packages/daemon/internal/daemon/notice_exceptions.go:7) matches too")"
check "names the daemon's copies of the listener's two liveness windows with their sites" \
  "$(contains "$out" "its listenerSessionTTL 180000 ms (packages/daemon/internal/daemon/notice_exceptions.go:6) matches that lifetime and its listenerClaimStale 240000 ms (packages/daemon/internal/daemon/notice_exceptions.go:8) matches ClaimStaleAfter 240000 ms (packages/envoy/internal/session/claim.go:5)$")"
# This assertion is a test of the check only while the fixture above keeps writing a
# MIN_PASS_DEADLINE_MS line into every tree: delete that line and this passes vacuously.
check "the pass-deadline floor is not quoted anywhere" \
  "$(is "$(contains "$out" 'PASS_DEADLINE')" false)"

echo "case: three intervals alone equal to the record lifetime"
# Three intervals with nothing else would land the third refresh at the instant the record
# expires; the call budget is what puts the sum past the lifetime, so this case proves the budget
# is counted. It is not the boundary itself - the next case is.
root=$(fixture equal 40_000 12_000 2_000 400 2 '2 * time.Minute' '40 * time.Second' 3)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the sum, the lifetime and both sites" \
  "$(contains "$out" '3 refresh intervals of 40000 ms (packages/envoy-client/src/envoy-settings.ts:4) plus one listener call.s 4400 ms (2 attempts of 2000 ms, packages/envoy-client/src/transport.ts:1, with 400 ms between them, packages/envoy-client/src/transport.ts:2; the attempt count is at packages/envoy-client/src/transport.ts:6) are 124400 ms, past the session record lifetime of 120000 ms (packages/envoy/internal/session/registry.go:4)')"
check "says the third refresh does not complete before the record expires" \
  "$(contains "$out" 'the third refresh does not complete before the record expires')"
check "says the record expires from the listener's acceptance, not the attempt" \
  "$(contains "$out" 'still owes its round trip')"
check "names the remedy on either side in whole seconds, with the call budget taken off the interval" \
  "$(contains "$out" 'Lower DEFAULT_HEARTBEAT_MS to at most 38000 ms, the longest whole second that leaves the sum short of the lifetime (and the daemon.s Heartbeat and pluginHeartbeat with it), or raise the registry.s default ttl to at least 125000 ms, the shortest whole second past the sum')"
check "counts the problem" "$(contains "$out" '::error::1 problem(s)')"

echo "case: three intervals plus the call budget exactly equal to the record lifetime"
# The boundary itself, reached by moving the call budget and not the heartbeat: a timeout of
# 29 800 ms makes the budget 2 x 29 800 + 400 = 60 000 ms, and three intervals of 40 000 ms plus it
# are the lifetime of 180 000 ms to the millisecond. Every other rule passes on this tree, so the
# renewal rule alone must refuse it.
root=$(fixture tie 40_000 12_000 29_800)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the sum as equal to the lifetime, not past it, with every site" \
  "$(contains "$out" '3 refresh intervals of 40000 ms (packages/envoy-client/src/envoy-settings.ts:4) plus one listener call.s 60000 ms (2 attempts of 29800 ms, packages/envoy-client/src/transport.ts:1, with 400 ms between them, packages/envoy-client/src/transport.ts:2; the attempt count is at packages/envoy-client/src/transport.ts:6) are 180000 ms, equal to the session record lifetime of 180000 ms (packages/envoy/internal/session/registry.go:4)')"
check "says it rejects the tie, not only what is past it" \
  "$(contains "$out" 'the sum must fall strictly short of it: equal is refused as surely as past, because either way the third refresh does not complete before the record expires')"
check "says why equality is not enough" \
  "$(contains "$out" 'so even at equality it lands no earlier than the instant of expiry')"
check "names remedies that leave the sum short of the lifetime" \
  "$(contains "$out" 'Lower DEFAULT_HEARTBEAT_MS to at most 39000 ms, .* or raise the registry.s default ttl to at least 181000 ms')"
check "and nothing else fails with it" "$(contains "$out" '::error::1 problem(s)')"

echo "case: the tie with its suggested maximum heartbeat applied"
root=$(fixture tie-heartbeat-remedy 39_000 12_000 29_800 400 2 '3 * time.Minute' \
  '39 * time.Second' 5 '39 * time.Second')
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "the sum falls short of the lifetime" \
  "$(contains "$out" 'are 177000 ms, strictly inside the session record lifetime 180000 ms')"

echo "case: the tie with its suggested minimum lifetime applied"
root=$(fixture tie-ttl-remedy 40_000 12_000 29_800 400 2 '181 * time.Second')
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "the sum falls short of the lifetime" \
  "$(contains "$out" 'are 180000 ms, strictly inside the session record lifetime 181000 ms')"

echo "case: three intervals past the record lifetime"
root=$(fixture renewals 50_000 12_000 2_000 400 2 '2 * time.Minute' '50 * time.Second' 3 '50 * time.Second')
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both numbers and both sites" \
  "$(contains "$out" '3 refresh intervals of 50000 ms (packages/envoy-client/src/envoy-settings.ts:4) plus one listener call.s 4400 ms .* are 154400 ms, past the session record lifetime of 120000 ms (packages/envoy/internal/session/registry.go:4)')"
check "says what breaks in production" \
  "$(contains "$out" 'every other agent asking the listener for that pane is told there is no live session for it')"
check "counts the problem" "$(contains "$out" '::error::1 problem(s)')"

echo "case: a pass-deadline floor far above the heartbeat"
# The false refusal this check must not make: the floor bounds how long one pass may run, not the
# gap between two accepted registrations, so a tree that raises it still passes with the same
# renewal window.
root=$(fixture deadline-floor 40_000 200_000)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "the renewal window is still three intervals and one call" \
  "$(contains "$out" 'are 124400 ms, strictly inside the session record lifetime 180000 ms')"

echo "case: the daemon's copy of the interval drifted from the TypeScript one"
root=$(fixture daemon-copy 40_000 12_000 2_000 400 2 '3 * time.Minute' '41 * time.Second')
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both numbers and both sites" \
  "$(contains "$out" "the daemon's Heartbeat is 41000 ms (packages/daemon/internal/controller/liveness.go:5) while the interval panes actually refresh at is 40000 ms (packages/envoy-client/src/envoy-settings.ts:4)")"
check "says what breaks in production" \
  "$(contains "$out" 'report a live controller gone and hand the role elsewhere')"
check "names the remedy" \
  "$(contains "$out" 'Set Heartbeat to 40000 ms')"

echo "case: the daemon's second copy of the interval drifted from the TypeScript one"
root=$(fixture plugin-copy 40_000 12_000 2_000 400 2 '3 * time.Minute' '40 * time.Second' 5 '41 * time.Second')
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both numbers and both sites" \
  "$(contains "$out" "the daemon's pluginHeartbeat is 41000 ms (packages/daemon/internal/daemon/notice_exceptions.go:7) while the interval panes actually refresh at is 40000 ms (packages/envoy-client/src/envoy-settings.ts:4)")"
check "names the delay it is" \
  "$(contains "$out" 'pluginHeartbeat is noticeReholdDelays\[1\]')"
check "says what breaks" \
  "$(contains "$out" "the schedule the daemon's own test derives from those delays moves with it")"
check "names the remedy" \
  "$(contains "$out" 'Set pluginHeartbeat to 40000 ms')"
check "and nothing else fails with it" "$(contains "$out" '::error::1 problem(s)')"

echo "case: the daemon's staleness window short of the record lifetime"
root=$(fixture daemon-window 40_000 12_000 2_000 400 2 '3 * time.Minute' '40 * time.Second' 3)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "reads the multiple out of liveness.go and names both numbers and both sites" \
  "$(contains "$out" "the daemon's staleness window is 3 x Heartbeat = 120000 ms (packages/daemon/internal/controller/liveness.go:8, over the Heartbeat of 40000 ms at packages/daemon/internal/controller/liveness.go:5), short of the session record lifetime of 180000 ms (packages/envoy/internal/session/registry.go:4)")"
check "says what breaks in production" \
  "$(contains "$out" 'reports a live controller gone')"
check "names the remedy as a multiple at this heartbeat" \
  "$(contains "$out" 'Widen the window to at least 180000 ms - 5 x Heartbeat at this Heartbeat')"
check "says the boot timeout arm is out of reach" \
  "$(contains "$out" 'bootTimeout is deployment configuration this check cannot see')"

echo "case: a record lifetime written in seconds"
root=$(fixture ttl-seconds 40_000 12_000 2_000 400 2 '180 * time.Second')
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads 180 * time.Second as 180000 ms" \
  "$(contains "$out" 'inside the session record lifetime 180000 ms (packages/envoy/internal/session/registry.go:4)')"

echo "case: a daemon heartbeat written in minutes"
root=$(fixture heartbeat-minutes 60_000 12_000 2_000 400 2 '4 * time.Minute' '1 * time.Minute' 4 '1 * time.Minute')
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reads 1 * time.Minute as 60000 ms, equal to the TypeScript 60_000" \
  "$(contains "$out" "daemon's Heartbeat 60000 ms (packages/daemon/internal/controller/liveness.go:5) matches")"

echo "case: the heartbeat constant renamed"
root=$(fixture renamed)
sed -i 's/DEFAULT_HEARTBEAT_MS = /DEFAULT_REFRESH_MS = /' \
  "$root/packages/envoy-client/src/envoy-settings.ts"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and the pattern it needs" \
  "$(contains "$out" '^::error file=packages/envoy-client/src/envoy-settings.ts::packages/envoy-client/src/envoy-settings.ts declares no DEFAULT_HEARTBEAT_MS this check can read: no line matches ')"
check "says a renamed constant is this failure" "$(contains "$out" 'A renamed constant, a moved file')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: liveness.go missing"
root=$(fixture missing)
rm "$root/packages/daemon/internal/controller/liveness.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/daemon/internal/controller/liveness.go is missing: this check cannot prove the heartbeat margin without it')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: RETRY_DELAY_MS declared twice"
root=$(fixture ambiguous)
cat > "$root/packages/envoy-client/src/transport.ts" <<'TS'
const DEFAULT_TIMEOUT_MS = 2_000;
const RETRY_DELAY_MS = 400;
const RETRY_DELAY_MS = 900;

export function createEnvoyClient(config: EnvoyClientConfig): EnvoyClient {
  const attempts = config.retry === false ? 1 : 2;
  return { attempts };
}
TS
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both line numbers" \
  "$(contains "$out" 'packages/envoy-client/src/transport.ts declares RETRY_DELAY_MS more than once (packages/envoy-client/src/transport.ts:2, packages/envoy-client/src/transport.ts:3)')"
check "refuses to guess which is in force" \
  "$(contains "$out" 'cannot tell which number is in force')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a heartbeat written as an expression"
root=$(fixture expression)
sed -i 's/DEFAULT_HEARTBEAT_MS = 40_000;/DEFAULT_HEARTBEAT_MS = 40 * 1000;/' \
  "$root/packages/envoy-client/src/envoy-settings.ts"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/envoy-client/src/envoy-settings.ts declares no DEFAULT_HEARTBEAT_MS this check can read')"
check "names the forms it will not guess at" \
  "$(contains "$out" 'a leading zero, hexadecimal, an expression, a unit other than time.Second or time.Minute')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a retry delay written in hexadecimal"
root=$(fixture hexadecimal 40_000 12_000 2_000 0x190)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses 0x190 rather than reading it as 400" \
  "$(contains "$out" 'packages/envoy-client/src/transport.ts declares no RETRY_DELAY_MS this check can read')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a record lifetime written with the multiplication the other way round"
root=$(fixture ttl-reversed 40_000 12_000 2_000 400 2 'time.Minute * 3')
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and what it needed" \
  "$(contains "$out" "packages/envoy/internal/session/registry.go declares no OpenSessionRegistry's default ttl this check can read")"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a record lifetime written in a unit the check does not convert"
root=$(fixture ttl-hours 40_000 12_000 2_000 400 2 '1 * time.Hour')
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses time.Hour rather than guessing at its milliseconds" \
  "$(contains "$out" "packages/envoy/internal/session/registry.go declares no OpenSessionRegistry's default ttl this check can read")"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: the window's heartbeat multiple written another way"
root=$(fixture window-expression)
sed -i 's/return max(bootTimeout, 5\*Heartbeat)/return max(bootTimeout, Heartbeat+Heartbeat)/' \
  "$root/packages/daemon/internal/controller/liveness.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and what it needed" \
  "$(contains "$out" "packages/daemon/internal/controller/liveness.go declares no LivenessWindow's heartbeat multiple this check can read")"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a file that is not valid UTF-8"
root=$(fixture not-utf8)
printf 'package session\n\nfunc OpenSessionRegistry() {\n    opts := sessionRegistryOpts{replicas: 1, ttl: 3 * time.Minute} // \xff\n}\n' \
  > "$root/packages/envoy/internal/session/registry.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/envoy/internal/session/registry.go is not readable as UTF-8 text')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: a directory where envoy-settings.ts belongs"
# A directory, not a mode-000 file: root may read a file no mode forbids it, and the suite must
# behave the same under either user.
root=$(fixture settings-is-a-directory)
rm "$root/packages/envoy-client/src/envoy-settings.ts"
mkdir "$root/packages/envoy-client/src/envoy-settings.ts"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file it could not read" \
  "$(contains "$out" '^::error file=packages/envoy-client/src/envoy-settings.ts::packages/envoy-client/src/envoy-settings.ts cannot be read: ')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"
check "nothing is listed beside it" "$(is "$(contains "$out" 'problem(s)')" false)"

echo "case: a form feed in the comment above a declaration"
# `str.splitlines()` breaks on \f, U+2028, U+0085 and \v, which an editor does not, so a line
# number counted that way names a line the reader cannot open. The declaration below carries four
# newlines before it and so sits on line 5, whatever the comment holds.
root=$(fixture form-feed)
printf 'package controller\n\n// Heartbeat is the pane,\fthe interval it refreshes at.\n\nconst Heartbeat = 40 * time.Second\n\nfunc LivenessWindow(bootTimeout time.Duration) time.Duration {\n    return max(bootTimeout, 5*Heartbeat)\n}\n' \
  > "$root/packages/daemon/internal/controller/liveness.go"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "reports the line a reader opens, not one shifted by the form feed" \
  "$(contains "$out" "daemon's Heartbeat 40000 ms (packages/daemon/internal/controller/liveness.go:5) matches")"

echo "case: the daemon's copy of the record lifetime drifted from the registry's ttl"
root=$(fixture ttl-copy 40_000 12_000 2_000 400 2 '3 * time.Minute' '40 * time.Second' 5 \
  '40 * time.Second' '4 * time.Minute')
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both numbers and both sites" \
  "$(contains "$out" "the daemon's listenerSessionTTL is 240000 ms (packages/daemon/internal/daemon/notice_exceptions.go:6) while the listener's own session record lifetime is 180000 ms (packages/envoy/internal/session/registry.go:4)")"
check "names the remedy that would cause this drift" \
  "$(contains "$out" "raising the registry.s default ttl, the remedy this check names above")"
check "says what breaks" \
  "$(contains "$out" "the forward is refused, nothing is held, and the architect.s notice is dropped")"
check "names the remedy" \
  "$(contains "$out" 'Set listenerSessionTTL to 180000 ms')"
check "and nothing else fails with it" "$(contains "$out" '::error::1 problem(s)')"

echo "case: the daemon's copy of the claim staleness window drifted from ClaimStaleAfter"
root=$(fixture claim-copy 40_000 12_000 2_000 400 2 '3 * time.Minute' '40 * time.Second' 5 \
  '40 * time.Second' '3 * time.Minute' '4 * time.Minute' '5 * time.Minute')
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both numbers and both sites" \
  "$(contains "$out" "the daemon's listenerClaimStale is 300000 ms (packages/daemon/internal/daemon/notice_exceptions.go:8) while the listener's ClaimStaleAfter is 240000 ms (packages/envoy/internal/session/claim.go:5)")"
check "names the delay it feeds" \
  "$(contains "$out" "noticeReholdDelays. last delay is the longer of the two copies")"
check "names the remedy" \
  "$(contains "$out" 'Set listenerClaimStale to 240000 ms')"
check "and nothing else fails with it" "$(contains "$out" '::error::1 problem(s)')"

echo "case: claim.go missing"
root=$(fixture claim-missing)
rm "$root/packages/envoy/internal/session/claim.go"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file" \
  "$(contains "$out" 'packages/envoy/internal/session/claim.go is missing: this check cannot prove the heartbeat margin without it')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: the real repository passes"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "reports the margin, the daemon's agreement on both copies of the interval, and its two copies of the listener's windows" \
  "$(contains "$out" '^3 refresh intervals of .* (packages/envoy-client/src/envoy-settings.ts:.*) plus one listener call .* are .*, strictly inside the session record lifetime .* (packages/envoy/internal/session/registry.go:.*) covers that lifetime; its pluginHeartbeat .* (packages/daemon/internal/daemon/notice_exceptions.go:.*) matches too; its listenerSessionTTL .* (packages/daemon/internal/daemon/notice_exceptions.go:.*) matches that lifetime and its listenerClaimStale .* (packages/daemon/internal/daemon/notice_exceptions.go:.*) matches ClaimStaleAfter .* (packages/envoy/internal/session/claim.go:.*)$')"

summary "check-heartbeat-margin.sh"

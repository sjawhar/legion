#!/usr/bin/env bash
# Tests for `check-heartbeat-margin.sh`: the interval a pane refreshes its Envoy registration at
# leaves a margin inside the lifetime the listener keeps a session record for, the Legion daemon's
# copy of that interval agrees and its staleness window covers that lifetime, and the check refuses
# any tree it cannot read unambiguously. The failure it stops is a silent one — a live pane's
# record expires and every other agent is told there is no live session for it — so the suite pins
# the exit status and the wording the author of a drift has to act on, including that the third
# refresh must complete inside the lifetime rather than merely be attempted, and that a
# pass-deadline floor far above the heartbeat is no longer refused.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real script
# into it, so the script under test is the file CI runs. The fixtures' numbers are invented and are
# never the repository's real ones: a hardcoded copy of any real number in the check would fail
# this suite.
#
# Run from anywhere: .github/scripts/check-heartbeat-margin.test.sh
# CI runs it in the Tests workflow (pr-and-main.yaml, job test).
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-heartbeat-margin.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name> [heartbeat-ms] [pass-deadline-floor-ms] [timeout-ms] [retry-delay-ms] [attempts]
#         [record-lifetime] [daemon-heartbeat] [window-heartbeats]: a tree that passes — three
# refresh intervals plus one listener call, and the worst case between registrations, both inside
# the record lifetime, the daemon's copy of the interval equal to the TypeScript one and its
# window covering that lifetime. The pass-deadline floor is written because the real file declares
# one; the check must not read it.
# Echoes its root.
fixture() {
  local root="$work/$1" heartbeat=${2:-40_000} deadline=${3:-12_000} timeout=${4:-2_000}
  local retry=${5:-400} attempts=${6:-2} lifetime=${7:-3 * time.Minute}
  local daemon_heartbeat=${8:-40 * time.Second} window=${9:-5}
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/packages/envoy-client/src" \
    "$root/packages/envoy/internal/session" "$root/packages/daemon/internal/controller"
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
check "names the worst case as two intervals and one call, with the deadline bounding neither" \
  "$(contains "$out" 'the worst case between registrations - two intervals and one such call, the pass deadline bounding neither - is 84400 ms')"
check "names the record lifetime and its site" \
  "$(contains "$out" 'both inside the session record lifetime 180000 ms (packages/envoy/internal/session/registry.go:4)')"
check "names the daemon's heartbeat and window with their sites" \
  "$(contains "$out" "daemon's Heartbeat 40000 ms (packages/daemon/internal/controller/liveness.go:5) matches and its window 5 x Heartbeat = 200000 ms (packages/daemon/internal/controller/liveness.go:8) covers that lifetime$")"
check "the pass-deadline floor is not quoted anywhere" \
  "$(is "$(contains "$out" 'PASS_DEADLINE')" false)"

echo "case: three intervals exactly equal to the record lifetime"
# The case the old rule passed: the third refresh is attempted at the instant the record expires
# and has not completed, so it no longer renews anything.
root=$(fixture equal 40_000 12_000 2_000 400 2 '2 * time.Minute' '40 * time.Second' 3)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the sum, the lifetime and both sites" \
  "$(contains "$out" '3 refresh intervals of 40000 ms (packages/envoy-client/src/envoy-settings.ts:4) plus one listener call.s 4400 ms (2 attempts of 2000 ms, packages/envoy-client/src/transport.ts:1, with 400 ms between them, packages/envoy-client/src/transport.ts:2; the attempt count is at packages/envoy-client/src/transport.ts:6) are 124400 ms, past the session record lifetime of 120000 ms (packages/envoy/internal/session/registry.go:4)')"
check "says the third refresh does not complete inside the lifetime" \
  "$(contains "$out" 'the third refresh does not complete inside the lifetime')"
check "says the record expires from the listener's acceptance, not the attempt" \
  "$(contains "$out" 'still owes its round trip')"
check "names the remedy on either side, with the call budget taken off the interval" \
  "$(contains "$out" 'Lower DEFAULT_HEARTBEAT_MS to at most 38533 ms, or raise the registry.s default ttl to at least 124400 ms')"
check "equality is no longer called the rule met exactly" \
  "$(is "$(contains "$out" 'Equality is the rule met exactly')" false)"
check "and no longer tells the maintainer not to widen either number" \
  "$(is "$(contains "$out" "do not .fix. it by widening")" false)"
check "counts the problem" "$(contains "$out" '::error::1 problem(s)')"

echo "case: three intervals past the record lifetime"
root=$(fixture renewals 50_000 12_000 2_000 400 2 '2 * time.Minute' '50 * time.Second' 3)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both numbers and both sites" \
  "$(contains "$out" '3 refresh intervals of 50000 ms (packages/envoy-client/src/envoy-settings.ts:4) plus one listener call.s 4400 ms .* are 154400 ms, past the session record lifetime of 120000 ms (packages/envoy/internal/session/registry.go:4)')"
check "says what breaks in production" \
  "$(contains "$out" 'every other agent asking the listener for that pane is told there is no live session for it')"
check "counts the problem" "$(contains "$out" '::error::1 problem(s)')"

echo "case: the worst case between registrations past the record lifetime"
root=$(fixture worst-case 40_000 12_000 90_000)
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the sum as two intervals and one call, with every site" \
  "$(contains "$out" 'the worst case between two accepted registrations is 260400 ms - two refresh intervals of 40000 ms (packages/envoy-client/src/envoy-settings.ts:4), plus one listener call.s 180400 ms (2 attempts of 90000 ms, packages/envoy-client/src/transport.ts:1, with 400 ms between them, packages/envoy-client/src/transport.ts:2; the attempt count is at packages/envoy-client/src/transport.ts:6)')"
check "names the lifetime and its site" \
  "$(contains "$out" 'past the session record lifetime of 180000 ms (packages/envoy/internal/session/registry.go:4)')"
check "says why two intervals and not the pass deadline" \
  "$(contains "$out" 'Two intervals, not one interval and the pass deadline: a stalled pass delays the refresh only until the heartbeat.s backstop posts it')"
check "names the backstop that bounds the gap" \
  "$(contains "$out" 'ensureHeartbeat., packages/pi-envoy/extensions/envoy.ts), whatever the pass deadline is')"
check "says what breaks in production" \
  "$(contains "$out" 'one stalled pass is enough for a live pane.s record to expire')"
check "names the remedy" \
  "$(contains "$out" 'raise the registry.s default ttl to at least 260400 ms')"
check "and the renewal rule, which this call budget also breaks, is reported beside it" \
  "$(contains "$out" '::error::2 problem(s)')"

echo "case: a pass-deadline floor far above the heartbeat"
# The false refusal this change removes: the floor bounds how long one pass may run, not the gap
# between two accepted registrations, so a tree that raises it still passes.
root=$(fixture deadline-floor 40_000 200_000)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "the worst case is still two intervals and one call" \
  "$(contains "$out" 'the worst case between registrations - two intervals and one such call, the pass deadline bounding neither - is 84400 ms')"

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
root=$(fixture heartbeat-minutes 60_000 12_000 2_000 400 2 '4 * time.Minute' '1 * time.Minute' 4)
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

echo "case: the real repository passes"
set +e
out=$("$check_script" 2>&1)
status=$?
set -e
check "exits 0" "$(is "$status" 0)"
check "reports both margins and the daemon's agreement" \
  "$(contains "$out" '^3 refresh intervals of .* (packages/envoy-client/src/envoy-settings.ts:.*) plus one listener call .* are .*, and the worst case between registrations - two intervals and one such call, the pass deadline bounding neither - is .*; both inside the session record lifetime .* (packages/envoy/internal/session/registry.go:.*) covers that lifetime$')"

summary "check-heartbeat-margin.sh"

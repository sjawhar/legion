#!/usr/bin/env bash
# A live Oh My Pi pane keeps its Envoy registration fresh by refreshing it: the pane's session
# record in the listener's `envoy_sessions` bucket carries the time of its last registration, and
# a session whose last registration is older than the live window is no longer live to the
# listener - every list, lookup and send that asks for live sessions alone leaves it out. The two
# numbers that decide whether a working pane keeps reading live are set in different languages and
# different packages - the refresh interval (`DEFAULT_HEARTBEAT_MS`,
# packages/envoy-client/src/envoy-settings.ts) and the live window (`LiveWindow`,
# packages/envoy/internal/session/state.go) - so moving one without the other is the drift this
# check stops. When it is not stopped, a pane whose process is still running reads unresponsive,
# and the messages other agents send it are stored rather than delivered, with nothing logged.
#
# Three things must hold, and this check fails unless all three do:
#
#   1. Three refresh intervals, plus one listener call's budget, fit strictly inside the live
#      window: a sum equal to the window fails as surely as one past it. A registration is renewed
#      at a third of the window, so two whole refreshes may fail before a working pane stops
#      reading live - but the third must *complete* inside the window, not merely be attempted.
#      The window is measured from the registration the listener accepted, while the third refresh
#      is only posted three intervals after that acceptance and still owes its round trip, and
#      `setInterval` only ever fires late; at equality the third refresh's budgeted completion
#      reaches the instant the window closes, leaving no scheduling margin. With a 40 000 ms
#      interval, a 60 000 ms call budget and a 180 000 ms window, the third refresh is dispatched
#      at 120 000 ms and its budget runs out at 180 000 ms. The call budget is that round trip:
#      every attempt's timeout plus the retry delays between them
#      (packages/envoy-client/src/transport.ts).
#
#      The pass deadline is not in that sum, and is not forgotten. A pass that stalls does not hold
#      the refresh back until its deadline: the heartbeat's own backstop posts the refresh at the
#      first tick where a pass is still running and the last accepted registration is a whole
#      interval old (`ensureHeartbeat`, packages/pi-envoy/extensions/envoy.ts), whatever the
#      deadline is. So the gap a stalled pass can open between two accepted registrations is at
#      most two intervals and one listener call, which the sum above already covers.
#      `MIN_PASS_DEADLINE_MS` only floors how long one pass may run, and raising it must not be
#      refused by this check.
#   2. The daemon's copy of the interval (`pluginHeartbeat`,
#      packages/daemon/internal/daemon/notice_exceptions.go) equals the TypeScript one.
#      That file is read because the constant is a real delay and not a comment: it is
#      `noticeReholdDelays[1]`, so the second re-hold of a notice the listener could not forward
#      goes one pane heartbeat after the first, and the schedule the daemon's own test derives
#      from those delays moves with it. A copy that drifts re-sends that notice at the wrong
#      distance - too soon, before a relaunched pane has registered, or too late, holding the
#      architect's notice past the window it could have been caught in.
#   3. The daemon's hand-copies of the listener's two liveness windows (`listenerSessionTTL` and
#      `listenerClaimStale`, packages/daemon/internal/daemon/notice_exceptions.go) equal the
#      windows themselves - the live window (`LiveWindow`,
#      packages/envoy/internal/session/state.go) and `ClaimStaleAfter`
#      (packages/envoy/internal/session/claim.go, read for this rule alone). Those copies are
#      `noticeReholdDelays`' last delay, which is when the last copy of a notice the listener
#      could not forward is finally sent and, if still refused, dropped. Rule 1's own remedy says
#      to "raise the live window" - the change that desynchronises them - and a copy left behind
#      sends that last notice while the stopped session's registration still reads live, so it is
#      refused and dropped while the listener still names a holder that has stopped answering.
#
# What this check cannot see:
#
# - It compares the declarations in the source, not the live deployment: a listener given another
#   live window through the registry's options (`session.WithSessionLiveWindow`) runs on that one,
#   and no file here records it. The `envoy_sessions` bucket's own maximum age is a different
#   number - the backstop that removes a record no reaper deleted, not what makes a session live -
#   and this check does not read it; `nats kv info envoy_sessions` reports it.
# - It cannot see a pane started with `ENVOY_HEARTBEAT_MS` set: that overrides the interval for
#   that pane alone, and no file records it.
# - It keeps no copy of any of these numbers. Each is read from the one file that defines it, and
#   a declaration this check cannot read for certain — renamed, missing, declared twice, or written
#   in a form it would have to guess at — is fatal and names the file and what it needed.
#
# Run from anywhere: .github/scripts/check-heartbeat-margin.sh
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path
from typing import NamedTuple, NoReturn

SETTINGS_FILE = "packages/envoy-client/src/envoy-settings.ts"
TRANSPORT_FILE = "packages/envoy-client/src/transport.ts"
STATE_FILE = "packages/envoy/internal/session/state.go"
NOTICE_FILE = "packages/daemon/internal/daemon/notice_exceptions.go"
CLAIM_FILE = "packages/envoy/internal/session/claim.go"

# A plain decimal, with the digit separators the language writes: TypeScript writes these
# constants `100_000`, so an underscore between digit runs is accepted and removed before the
# number is read. A leading zero, hexadecimal, or an expression is not accepted - each would have
# to be guessed at, and a guess here is a number this check reports agreement about without
# having established it.
DECIMAL = r"(?:0|[1-9][0-9]*(?:_[0-9]+)*)"
# A Go duration written as `<n> * time.Second` or `<n> * time.Minute`, the two units these
# constants use. `time.Minute * 3`, `300*time.Second/2` and a named constant are refused.
GO_DURATION = rf"({DECIMAL})\s*\*\s*time\.(Second|Minute)"

HEARTBEAT_CONST = "DEFAULT_HEARTBEAT_MS"
TIMEOUT_CONST = "DEFAULT_TIMEOUT_MS"
RETRY_DELAY_CONST = "RETRY_DELAY_MS"

HEARTBEAT_PATTERN = rf"^\s*export\s+const\s+{HEARTBEAT_CONST}\s*=\s*({DECIMAL})\s*;\s*(?://.*)?$"
TIMEOUT_PATTERN = rf"^\s*const\s+{TIMEOUT_CONST}\s*=\s*({DECIMAL})\s*;\s*(?://.*)?$"
RETRY_DELAY_PATTERN = rf"^\s*const\s+{RETRY_DELAY_CONST}\s*=\s*({DECIMAL})\s*;\s*(?://.*)?$"
# How many times one listener call is attempted when it is allowed to retry: the second branch of
# `const attempts = config.retry === false ? 1 : 2;`.
ATTEMPTS_PATTERN = (
    r"^\s*const\s+attempts\s*=\s*config\.retry\s*===\s*false\s*\?\s*"
    rf"{DECIMAL}\s*:\s*({DECIMAL})\s*;\s*(?://.*)?$"
)
# The live window, as session.LiveWindow declares it: the age past which a session's last
# registration no longer reads live. An option moves it for one listener
# (session.WithSessionLiveWindow); this is the tree's own number.
LIVE_WINDOW_PATTERN = rf"^\s*LiveWindow\s*=\s*{GO_DURATION}\s*(?://.*)?$"
# The daemon's second copy of the refresh interval, one member of the const block it is declared
# in and the second of `noticeReholdDelays`.
PLUGIN_HEARTBEAT_PATTERN = rf"^\s*pluginHeartbeat\s*=\s*{GO_DURATION}\s*(?://.*)?$"
# The daemon's hand-copies of the listener's two liveness windows, the other two members of that
# same const block, and `ClaimStaleAfter` itself, which is one of the windows they copy.
LISTENER_TTL_PATTERN = rf"^\s*listenerSessionTTL\s*=\s*{GO_DURATION}\s*(?://.*)?$"
LISTENER_CLAIM_STALE_PATTERN = rf"^\s*listenerClaimStale\s*=\s*{GO_DURATION}\s*(?://.*)?$"
CLAIM_STALE_AFTER_PATTERN = rf"^\s*const\s+ClaimStaleAfter\s*=\s*{GO_DURATION}\s*(?://.*)?$"

MILLIS = {"Second": 1_000, "Minute": 60_000}
# A registration is renewed at a third of the live window, so two whole refreshes may fail before
# a working session stops reading live. This is the rule the margin is held to, not a copy of any
# number in the tree.
RENEWALS = 3

problems: list[tuple[str, int | None, str]] = []


def one_line(value: object) -> str:
    """A value quoted into a message cannot carry newlines, or it reaches the Actions log as a
    line of its own."""
    return " ".join(str(value).split())


def problem(path: str, line: int | None, message: str) -> None:
    problems.append((path, line, message))


def fatal(path: str, message: str) -> NoReturn:
    """A tree this check cannot read at all: one `::error` line and done. There is nothing to
    list it beside - every finding after it would be a guess about a file or a declaration this
    check never saw."""
    print(f"::error file={path}::{message}", file=sys.stderr)
    raise SystemExit(1)


def load(path: str) -> list[str]:
    """The file's lines, split on `\\n` alone with a trailing `\\r` dropped, so a line number this
    check reports is the one an editor opens. `str.splitlines()` also breaks on U+2028, U+0085,
    `\\v` and `\\f`, each of which would shift every number below it."""
    try:
        text = Path(path).read_bytes().decode("utf-8")
    except FileNotFoundError:
        fatal(path, f"{path} is missing: this check cannot prove the heartbeat margin without it")
    except UnicodeDecodeError as error:
        fatal(path, f"{path} is not readable as UTF-8 text: {one_line(error)}")
    except OSError as error:
        fatal(path, f"{path} cannot be read: {one_line(error)}")
    return [line.removesuffix("\r") for line in text.split("\n")]


class Declaration(NamedTuple):
    """A number read out of the tree, in milliseconds, and where a reader opens it."""

    value: int
    path: str
    line: int

    @property
    def site(self) -> str:
        return f"{self.path}:{self.line}"


def match_once(path: str, lines: list[str], what: str, pattern: str) -> tuple[int, re.Match[str]]:
    """The one line matching pattern; a tree with none or more than one is fatal."""
    matches = [
        (number, match)
        for number, line in enumerate(lines, start=1)
        if (match := re.match(pattern, line))
    ]
    if not matches:
        fatal(
            path,
            f"{path} declares no {what} this check can read: no line matches {pattern}. A renamed "
            f"constant, a moved file, or the number written another way (digit separators beyond "
            f"a plain decimal's, a leading zero, hexadecimal, an expression, a unit other than "
            f"time.Second or time.Minute) is this failure. Restore a declaration in the form "
            f"above, or update this check's pattern with the sites that moved",
        )
    if len(matches) > 1:
        where = ", ".join(f"{path}:{number}" for number, _ in matches)
        fatal(
            path,
            f"{path} declares {what} more than once ({where}), so this check cannot tell which "
            f"number is in force and refuses to read the harmless one. Leave exactly one "
            f"declaration",
        )
    return matches[0]


def millis(path: str, lines: list[str], what: str, pattern: str) -> Declaration:
    """A TypeScript millisecond constant, or a plain count, read as a number."""
    line, match = match_once(path, lines, what, pattern)
    return Declaration(int(match.group(1).replace("_", "")), path, line)


def duration(path: str, lines: list[str], what: str, pattern: str) -> Declaration:
    """A Go `<n> * time.Second` or `<n> * time.Minute` duration, in milliseconds."""
    line, match = match_once(path, lines, what, pattern)
    count = int(match.group(1).replace("_", ""))
    return Declaration(count * MILLIS[match.group(2)], path, line)


settings_lines = load(SETTINGS_FILE)
transport_lines = load(TRANSPORT_FILE)
state_lines = load(STATE_FILE)
notice_lines = load(NOTICE_FILE)
claim_lines = load(CLAIM_FILE)

heartbeat = millis(SETTINGS_FILE, settings_lines, HEARTBEAT_CONST, HEARTBEAT_PATTERN)
timeout = millis(TRANSPORT_FILE, transport_lines, TIMEOUT_CONST, TIMEOUT_PATTERN)
retry_delay = millis(TRANSPORT_FILE, transport_lines, RETRY_DELAY_CONST, RETRY_DELAY_PATTERN)
attempts = millis(TRANSPORT_FILE, transport_lines, "the retrying attempt count", ATTEMPTS_PATTERN)
live_window = duration(STATE_FILE, state_lines, "LiveWindow", LIVE_WINDOW_PATTERN)
plugin_heartbeat = duration(NOTICE_FILE, notice_lines, "pluginHeartbeat", PLUGIN_HEARTBEAT_PATTERN)
listener_ttl = duration(NOTICE_FILE, notice_lines, "listenerSessionTTL", LISTENER_TTL_PATTERN)
listener_claim_stale = duration(
    NOTICE_FILE, notice_lines, "listenerClaimStale", LISTENER_CLAIM_STALE_PATTERN
)
claim_stale = duration(CLAIM_FILE, claim_lines, "ClaimStaleAfter", CLAIM_STALE_AFTER_PATTERN)

# One listener call: every attempt's timeout, plus one retry delay between each pair of them.
call_budget = attempts.value * timeout.value + max(attempts.value - 1, 0) * retry_delay.value
renewal_window = RENEWALS * heartbeat.value + call_budget
# The remedies are whole seconds, because the numbers they move are Go durations this check reads
# only as whole seconds or minutes: the daemon's copy of the interval, and the live window. The
# interval is the longest whole second whose renewal window still falls short of the live window;
# the window is the shortest whole second past the renewal window.
SECOND = MILLIS["Second"]
max_heartbeat = (live_window.value - call_budget - 1) // RENEWALS // SECOND * SECOND
min_window = (renewal_window // SECOND + 1) * SECOND

if renewal_window >= live_window.value:
    relation = "equal to" if renewal_window == live_window.value else "past"
    problem(
        SETTINGS_FILE,
        heartbeat.line,
        f"{RENEWALS} refresh intervals of {heartbeat.value} ms ({heartbeat.site}) plus one "
        f"listener call's {call_budget} ms ({attempts.value} attempts of {timeout.value} ms, "
        f"{timeout.site}, with {retry_delay.value} ms between them, {retry_delay.site}; the "
        f"attempt count is at {attempts.site}) are {renewal_window} ms, {relation} the session "
        f"live window of {live_window.value} ms ({live_window.site}), and the sum must fall "
        f"strictly short of it: equal is refused as surely as past, because either way the third "
        f"refresh does not complete before the window closes. The window is measured from the "
        f"registration the listener accepted, while the third refresh is only posted {RENEWALS} "
        f"intervals later and still owes its round trip - and `setInterval` fires late - so even "
        f"at equality it lands no earlier than the instant the window closes. A pane whose "
        f"process is still running then reads unresponsive after two failed refreshes, and from "
        f"then on every list, lookup and send that asks for live sessions alone leaves it out - "
        f"its messages are stored rather than delivered, with nothing logged. Lower "
        f"{HEARTBEAT_CONST} to at most {max_heartbeat} ms, the longest whole second that leaves "
        f"the sum short of the window (and the daemon's pluginHeartbeat with it), or raise the "
        f"live window to at least {min_window} ms, the shortest whole second past the sum (and "
        f"the daemon's listenerSessionTTL with it)",
    )

if plugin_heartbeat.value != heartbeat.value:
    problem(
        NOTICE_FILE,
        plugin_heartbeat.line,
        f"the daemon's pluginHeartbeat is {plugin_heartbeat.value} ms ({plugin_heartbeat.site}) "
        f"while the interval panes actually refresh at is {heartbeat.value} ms ({heartbeat.site}): "
        f"pluginHeartbeat is noticeReholdDelays[1], so the second re-hold of a notice the "
        f"listener could not forward goes that far after the first - too soon and the copy is "
        f"spent before a relaunched pane has registered, too late and the architect's notice "
        f"waits past the window it could have been caught in - and the schedule the daemon's own "
        f"test derives from those delays moves with it. Set pluginHeartbeat to "
        f"{heartbeat.value} ms - it is a copy of {HEARTBEAT_CONST} and has no independent value",
    )

if listener_ttl.value != live_window.value:
    problem(
        NOTICE_FILE,
        listener_ttl.line,
        f"the daemon's listenerSessionTTL is {listener_ttl.value} ms ({listener_ttl.site}) while "
        f"the listener's own live window is {live_window.value} ms ({live_window.site}): it is a "
        f"hand-copy of that window, and noticeReholdDelays' last delay is taken from it, so the "
        f"last copy of a notice the listener could not forward is sent at the wrong distance from "
        f"the moment a stopped session's registration stops reading live. A copy left behind - "
        f"which raising the live window, the remedy this check names above, is what does - sends "
        f"that last copy while the listener still names the stopped session as the holder, so the "
        f"forward is refused, nothing is held, and the architect's notice is dropped. Set "
        f"listenerSessionTTL to {live_window.value} ms - it is a copy of session.LiveWindow and "
        f"has no independent value",
    )

if listener_claim_stale.value != claim_stale.value:
    problem(
        NOTICE_FILE,
        listener_claim_stale.line,
        f"the daemon's listenerClaimStale is {listener_claim_stale.value} ms "
        f"({listener_claim_stale.site}) while the listener's ClaimStaleAfter is "
        f"{claim_stale.value} ms ({claim_stale.site}): it is a hand-copy of that window, and "
        f"noticeReholdDelays' last delay is the longer of the two copies, so a copy left behind "
        f"sends the last copy of an unforwardable notice before the stopped holder's claim has "
        f"gone stale - the forward is refused, nothing is held, and the architect's notice is "
        f"dropped - while one left ahead holds it past the window it could have been caught in. "
        f"Set listenerClaimStale to {claim_stale.value} ms - it is a copy of ClaimStaleAfter and "
        f"has no independent value",
    )

for path, line, message in problems:
    location = f"file={path}" + (f",line={line}" if line is not None else "")
    print(f"::error {location}::{message}", file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} problem(s): the registration refresh interval and the session "
        f"live window do not demonstrably leave a margin",
        file=sys.stderr,
    )
    raise SystemExit(1)

print(
    f"{RENEWALS} refresh intervals of {heartbeat.value} ms ({heartbeat.site}) plus one listener "
    f"call {call_budget} ms ({attempts.value} attempts of {timeout.value} ms, {timeout.site}, "
    f"{retry_delay.value} ms apart, {retry_delay.site}; attempts at {attempts.site}) are "
    f"{renewal_window} ms, strictly inside the session live window {live_window.value} ms "
    f"({live_window.site}); the daemon's pluginHeartbeat {plugin_heartbeat.value} ms "
    f"({plugin_heartbeat.site}) matches the interval; its listenerSessionTTL "
    f"{listener_ttl.value} ms ({listener_ttl.site}) matches that window and its "
    f"listenerClaimStale {listener_claim_stale.value} ms ({listener_claim_stale.site}) matches "
    f"ClaimStaleAfter {claim_stale.value} ms ({claim_stale.site})"
)
PY

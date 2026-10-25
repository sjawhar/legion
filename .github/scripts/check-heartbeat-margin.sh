#!/usr/bin/env bash
# A live Oh My Pi pane keeps its Envoy registration by refreshing it: the pane's session record in
# the listener's `envoy_sessions` bucket expires on its own, and every refresh renews it. The two
# numbers that decide whether a live pane stays registered are set in different languages and
# different packages — the refresh interval (`DEFAULT_HEARTBEAT_MS`,
# packages/envoy-client/src/envoy-settings.ts) and the record lifetime (`OpenSessionRegistry`'s
# default `ttl`, packages/envoy/internal/session/registry.go) — so moving one without the other is
# the drift this check stops. When it is not stopped, a pane whose process is still running has its
# record expire, and every other agent asking the listener for it is told there is no live session:
# messages to it are refused, its role lookups answer gone, and nothing logs an error.
#
# Three things must hold, and this check fails unless all three do:
#
#   1. Three refresh intervals fit inside the record lifetime. A record that expires unless
#      refreshed is renewed at a third of its lifetime, so two whole refreshes may fail before a
#      live pane is dropped. "Fit inside" is `<=`: at the values in the tree today the two are
#      equal, which is the renewal rule met exactly, not broken.
#   2. One refresh interval, plus one pass deadline, plus one listener call's budget fit inside it
#      too. That is the real worst case between two accepted registrations: a refresh held behind
#      a serial-queue task abandoned at its deadline, and then one listener call that takes its
#      whole budget. The pass deadline is the effective one, `max(DEFAULT_HEARTBEAT_MS,
#      DEFAULT_PASS_DEADLINE_MS)` (`envoySettings`), and the call budget is every attempt's timeout
#      plus the retry delays between them (packages/envoy-client/src/transport.ts).
#   3. The Legion daemon's own copy of the interval (`Heartbeat`,
#      packages/daemon/internal/controller/liveness.go) equals the TypeScript one, and its
#      staleness window is at least the record lifetime. The window is
#      `max(bootTimeout, N*Heartbeat)`; `N` is read out of `liveness.go` rather than assumed, and
#      only the heartbeat arm is checked, because `bootTimeout` is deployment configuration this
#      check cannot see. A daemon window narrower than the record lifetime reports a live
#      controller gone.
#
# What this check cannot see:
#
# - It compares the defaults in the source, not the live deployment. `bus.EnsureKeyValue` creates
#   the `envoy_sessions` bucket only when it does not exist
#   (packages/envoy/internal/bus/kv.go:27-37), and `OpenSessionRegistry` then adopts whatever
#   `MaxAge` the live bucket reports (packages/envoy/internal/session/registry.go), so a bucket
#   created by an older build keeps its old lifetime however this constant moves. The live value is
#   in the listener's boot log, which names the record lifetime it resolved; `nats kv info
#   envoy_sessions` is the other way to read it.
# - It cannot see a pane started with `ENVOY_HEARTBEAT_MS` or `ENVOY_PASS_DEADLINE_MS` set: those
#   override the defaults for that pane alone, and no file records them.
# - It keeps no copy of any of these numbers. Each is read from the one file that defines it, and
#   a declaration this check cannot read for certain — renamed, missing, declared twice, or written
#   in a form it would have to guess at — is fatal and names the file and what it needed.
#
# Run from anywhere: .github/scripts/check-heartbeat-margin.sh
# CI runs it in the lint job of pr-and-main.yaml, and its tests in that workflow's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

python3 <<'PY'
import re
import sys
from pathlib import Path
from typing import NamedTuple, NoReturn

SETTINGS_FILE = "packages/envoy-client/src/envoy-settings.ts"
TRANSPORT_FILE = "packages/envoy-client/src/transport.ts"
REGISTRY_FILE = "packages/envoy/internal/session/registry.go"
LIVENESS_FILE = "packages/daemon/internal/controller/liveness.go"

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
PASS_DEADLINE_CONST = "DEFAULT_PASS_DEADLINE_MS"
TIMEOUT_CONST = "DEFAULT_TIMEOUT_MS"
RETRY_DELAY_CONST = "RETRY_DELAY_MS"

HEARTBEAT_PATTERN = rf"^\s*export\s+const\s+{HEARTBEAT_CONST}\s*=\s*({DECIMAL})\s*;\s*(?://.*)?$"
PASS_DEADLINE_PATTERN = (
    rf"^\s*export\s+const\s+{PASS_DEADLINE_CONST}\s*=\s*({DECIMAL})\s*;\s*(?://.*)?$"
)
TIMEOUT_PATTERN = rf"^\s*const\s+{TIMEOUT_CONST}\s*=\s*({DECIMAL})\s*;\s*(?://.*)?$"
RETRY_DELAY_PATTERN = rf"^\s*const\s+{RETRY_DELAY_CONST}\s*=\s*({DECIMAL})\s*;\s*(?://.*)?$"
# How many times one listener call is attempted when it is allowed to retry: the second branch of
# `const attempts = config.retry === false ? 1 : 2;`.
ATTEMPTS_PATTERN = (
    r"^\s*const\s+attempts\s*=\s*config\.retry\s*===\s*false\s*\?\s*"
    rf"{DECIMAL}\s*:\s*({DECIMAL})\s*;\s*(?://.*)?$"
)
# The record lifetime, as `OpenSessionRegistry` defaults it before its options are applied.
TTL_PATTERN = rf"^\s*opts\s*:=\s*sessionRegistryOpts\{{[^{{}}]*\bttl:\s*{GO_DURATION}\s*[,}}]"
DAEMON_HEARTBEAT_PATTERN = rf"^\s*const\s+Heartbeat\s*=\s*{GO_DURATION}\s*(?://.*)?$"
# How many heartbeats the daemon's staleness window is, out of `LivenessWindow`'s own expression.
WINDOW_FACTOR_PATTERN = (
    rf"^\s*return\s+max\(\s*bootTimeout\s*,\s*({DECIMAL})\s*\*\s*Heartbeat\s*\)\s*(?://.*)?$"
)

MILLIS = {"Second": 1_000, "Minute": 60_000}
# A record that expires unless refreshed is renewed at a third of its lifetime, so two whole
# refreshes may fail before a live session is dropped. This is the rule the margin is held to, not
# a copy of any number in the tree.
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
registry_lines = load(REGISTRY_FILE)
liveness_lines = load(LIVENESS_FILE)

heartbeat = millis(SETTINGS_FILE, settings_lines, HEARTBEAT_CONST, HEARTBEAT_PATTERN)
pass_deadline = millis(SETTINGS_FILE, settings_lines, PASS_DEADLINE_CONST, PASS_DEADLINE_PATTERN)
timeout = millis(TRANSPORT_FILE, transport_lines, TIMEOUT_CONST, TIMEOUT_PATTERN)
retry_delay = millis(TRANSPORT_FILE, transport_lines, RETRY_DELAY_CONST, RETRY_DELAY_PATTERN)
attempts = millis(TRANSPORT_FILE, transport_lines, "the retrying attempt count", ATTEMPTS_PATTERN)
lifetime = duration(REGISTRY_FILE, registry_lines, "OpenSessionRegistry's default ttl", TTL_PATTERN)
daemon_heartbeat = duration(LIVENESS_FILE, liveness_lines, "Heartbeat", DAEMON_HEARTBEAT_PATTERN)
window_factor = millis(
    LIVENESS_FILE, liveness_lines, "LivenessWindow's heartbeat multiple", WINDOW_FACTOR_PATTERN
)

# The deadline a pass actually gets: `envoySettings` takes the larger of the two
# (packages/envoy-client/src/envoy-settings.ts).
effective_deadline = max(heartbeat.value, pass_deadline.value)
# One listener call: every attempt's timeout, plus one retry delay between each pair of them.
call_budget = attempts.value * timeout.value + max(attempts.value - 1, 0) * retry_delay.value
renewals = RENEWALS * heartbeat.value
worst_case = heartbeat.value + effective_deadline + call_budget
daemon_window = window_factor.value * daemon_heartbeat.value

if renewals > lifetime.value:
    problem(
        SETTINGS_FILE,
        heartbeat.line,
        f"{RENEWALS} refresh intervals of {heartbeat.value} ms ({heartbeat.site}) are "
        f"{renewals} ms, past the session record lifetime of {lifetime.value} ms "
        f"({lifetime.site}): a pane whose process is still running has its record expire while "
        f"fewer than {RENEWALS} refreshes have had the chance to renew it, and from then on every "
        f"other agent asking the listener for that pane is told there is no live session for it - "
        f"messages to it are refused and its role lookups answer gone, with nothing logged. Lower "
        f"{HEARTBEAT_CONST} to at most {lifetime.value // RENEWALS} ms, or raise the registry's "
        f"default ttl to at least {renewals} ms (and the daemon's window with it). Equality is the "
        f"rule met exactly, not broken: {RENEWALS} x {HEARTBEAT_CONST} == the lifetime passes, and "
        f"is what the tree is tuned to - do not 'fix' it by widening either number",
    )

if worst_case > lifetime.value:
    problem(
        SETTINGS_FILE,
        heartbeat.line,
        f"the worst case between two accepted registrations is {worst_case} ms - one refresh "
        f"interval of {heartbeat.value} ms ({heartbeat.site}), plus the effective pass deadline of "
        f"{effective_deadline} ms (max of {HEARTBEAT_CONST} and {PASS_DEADLINE_CONST} of "
        f"{pass_deadline.value} ms, {pass_deadline.site}) for a refresh held behind a serial-queue "
        f"task abandoned at its deadline, plus one listener call's {call_budget} ms "
        f"({attempts.value} attempts of {timeout.value} ms, {timeout.site}, with "
        f"{retry_delay.value} ms between them, {retry_delay.site}; the attempt count is at "
        f"{attempts.site}) - past the session record lifetime of {lifetime.value} ms "
        f"({lifetime.site}): one slow pass is enough for a live pane's record to expire, and every "
        f"other agent asking the listener for that pane is then told there is no live session for "
        f"it, with nothing logged. Lower {HEARTBEAT_CONST}, {PASS_DEADLINE_CONST} or the call "
        f"budget until their sum is at most {lifetime.value} ms, or raise the registry's default "
        f"ttl to at least {worst_case} ms (and the daemon's window with it)",
    )

if daemon_heartbeat.value != heartbeat.value:
    problem(
        LIVENESS_FILE,
        daemon_heartbeat.line,
        f"the daemon's Heartbeat is {daemon_heartbeat.value} ms ({daemon_heartbeat.site}) while "
        f"the interval panes actually refresh at is {heartbeat.value} ms ({heartbeat.site}): the "
        f"daemon sizes its staleness window from its own copy, so a copy that is too small makes "
        f"it report a live controller gone and hand the role elsewhere, and one that is too large "
        f"makes it hold a dead controller live. Set Heartbeat to {heartbeat.value} ms - it is a "
        f"copy of {HEARTBEAT_CONST} and has no independent value",
    )

if daemon_window < lifetime.value:
    problem(
        LIVENESS_FILE,
        window_factor.line,
        f"the daemon's staleness window is {window_factor.value} x Heartbeat = {daemon_window} ms "
        f"({window_factor.site}, over the Heartbeat of {daemon_heartbeat.value} ms at "
        f"{daemon_heartbeat.site}), short of the session record lifetime of {lifetime.value} ms "
        f"({lifetime.site}): the daemon calls the controller gone while its record is still live "
        f"and its pane is still running, takes the controller role away from it and reports a live "
        f"controller gone. Widen the window to at least {lifetime.value} ms - "
        f"{-(-lifetime.value // daemon_heartbeat.value)} x Heartbeat at this Heartbeat. Only the "
        f"heartbeat arm of max(bootTimeout, N*Heartbeat) is checked: bootTimeout is deployment "
        f"configuration this check cannot see",
    )

for path, line, message in problems:
    location = f"file={path}" + (f",line={line}" if line is not None else "")
    print(f"::error {location}::{message}", file=sys.stderr)
if problems:
    print(
        f"::error::{len(problems)} problem(s): the registration refresh interval and the session "
        f"record lifetime do not demonstrably leave a margin",
        file=sys.stderr,
    )
    raise SystemExit(1)

print(
    f"{RENEWALS} refresh intervals of {heartbeat.value} ms ({heartbeat.site}) are {renewals} ms "
    f"and the worst case between registrations is {worst_case} ms - one interval, the effective "
    f"pass deadline {effective_deadline} ms (over {PASS_DEADLINE_CONST} {pass_deadline.value} ms, "
    f"{pass_deadline.site}) and one listener call {call_budget} ms ({attempts.value} attempts of "
    f"{timeout.value} ms, {timeout.site}, {retry_delay.value} ms apart, {retry_delay.site}; "
    f"attempts at {attempts.site}) - both inside the session record lifetime {lifetime.value} ms "
    f"({lifetime.site}); the daemon's Heartbeat {daemon_heartbeat.value} ms "
    f"({daemon_heartbeat.site}) matches and its window {window_factor.value} x Heartbeat = "
    f"{daemon_window} ms ({window_factor.site}) covers that lifetime"
)
PY

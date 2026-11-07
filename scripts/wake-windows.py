#!/usr/bin/env python3
"""The session windows a Legion daemon's own wakes cause, for the production reading of
dispatch://INFRA-976: `gateway-uncached-by-organization.py --sessions` takes the windows, and the
second mode counts the paced wakes of each minute against the rate and burst the lines name.

Two modes, because the windows are read by another program and a second section on standard
output would be read as a window line (`--sessions` takes every line of the file as
`<session id> <start> <end>`), so the per-minute reading is a flag and not a second section:

  wake-windows.py --state STATE_JSON [LOG] [--window MINUTES] [--since TIME] [--until TIME]
      One line per wake on standard output, `<session id> <start> <end>`, exactly the three
      space-separated fields `gateway-uncached-by-organization.py --sessions` reads, each time
      ISO-8601 UTC as that script's datetime.fromisoformat takes it. The window starts at the
      wake's own time and runs --window minutes (10 by default).

  wake-windows.py --minutes [LOG] [--since TIME] [--until TIME]
      One line per minute that holds a paced wake, `<minute> <paced wakes>`, then the highest of
      those minutes and the rate and burst the lines name, so a reader sees whether any minute
      held more paced wakes than the rate plus the burst. It reads the log alone and takes no
      --state.

LOG is the daemon's log, one JSON object per line as Go's slog JSON handler writes it, and
defaults to standard input (`kubectl -n legion logs <daemon pod> | wake-windows.py ...`). A line
that is not JSON, or whose `msg` is not `supervise: wake ran`, is skipped: a claim's own machine
logger puts `claim` on every line it writes, and the outbox writes its wake line with `claim`
too, so the message is what selects a wake and not the fields.

STATE_JSON is a saved body of `GET /legion/v1/state`. It maps each claim token to its session:
`daemon.project` is the project token, `issues` is keyed by issue and each issue carries
`architect` and `workers.<role>`, each of those a claim with `session` and, while it holds a
process, `locator.claim`. A `locator.claim` is preferred where the state gives one; otherwise the
token is composed as `legion-<project token>-<issue key, lower-cased>-<role>` (project `infra`,
issue `INFRA-976`, role `implementer` gives `legion-infra-infra-976-implementer`). The project
controller's own claim, `legion-<project token>-controller`, maps to `controllerLocator.sessionId`:
its wake takes no token and never waits, but the session it starts reads its context like any
other.

What counts where, and why the two differ:

  - Only a line whose `paced` is true counts toward the per-minute reading, because the rate
    bounds the paced wakes alone: an operator's request and the controller's own claim are let
    through, and a daemon with the rate at 0 paces nothing.
  - Every wake line gets a window, paced or not, because a woken session re-reads its context
    either way and so pays its uncached input either way.

One line per wake: a session woken twice gets the window of each wake, and the reader counts a
call once per window — `gateway-uncached-by-organization.py` counts a call that falls in any of a
session's windows once, so two overlapping windows of one session do not double-count it.

Standard error carries the reading's frame, never a window: each run of wakes with no gap above
--wave-gap, with how many wakes it held and its start and end, marked a wave once it holds at
least --wave-size of them (40, the wave `c-production` reads); the span the windows cover, which
is the pair of times to give `gateway-uncached-by-organization.py`; and every wake whose claim the
state does not name. Every window it can build is still printed, and the exit status is 1 when a
claim was unknown — a window silently missing is a reading that under-counts — and 1 when there
was nothing to print at all, with the reason on standard error; otherwise 0.

What it cannot show:

  - Tokens. It reads no spend log: it only says which session to read over which minutes, and
    `gateway-uncached-by-organization.py` says what that session spent.
  - Whether an organization was saturated. The windows hold the woken sessions alone, so the
    reading says nothing about the ordinary work of the same minute.
  - A wake the log does not hold: one before this change shipped, one dropped by a log the pod
    rotated, or one a handler below Info discarded.
  - The right session for a claim whose session changed between the wave and the state read: the
    state is read after the wave, so the window carries the session the claim holds now, and a
    claim retired since the wave is reported as unknown rather than guessed.
  - How long a wake waited. The lines carry `waited` and `waitedFor` (a duration, which Go's slog
    JSON handler writes as a number of nanoseconds), and the reader greps the log itself for them.
"""
import argparse
import collections
import datetime
import json
import sys

# The one message every paced lane writes when its wake runs: a role process launch ("launch"),
# the update task of a stale-plugin round ("plugin-task"), and the catch-up notice an outbox row
# holds ("catch-up"). All three write the same fields, so one pass over the log reads every lane.
MESSAGE = "supervise: wake ran"


def parse_time(text):
    """One ISO-8601 time as UTC. Go writes `Z` where Python wants `+00:00`, and a daemon that
    writes more than six fractional digits would be refused by fromisoformat, so the fraction is
    cut to microseconds."""
    text = text.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    head, dot, rest = text.partition(".")
    if dot:
        digits = 0
        while digits < len(rest) and rest[digits].isdigit():
            digits += 1
        text = head + "." + rest[:digits][:6] + rest[digits:]
    when = datetime.datetime.fromisoformat(text)
    if when.tzinfo is None:
        when = when.replace(tzinfo=datetime.UTC)
    return when.astimezone(datetime.UTC)


def claim_of(entry):
    """The claim record of an issue's architect or of one of its workers. The architect is the
    claim itself and a worker is the phase that holds one, so the nested `claim` is taken where
    the entry has one."""
    if not isinstance(entry, dict):
        return {}
    nested = entry.get("claim")
    return nested if isinstance(nested, dict) else entry


def sessions_by_claim(state):
    """Each claim token of the state with the session it holds: every worker, every architect, and
    the project controller. A claim with no session is left out — it woke nothing to read."""
    project = (state.get("daemon") or {}).get("project", "").lower()
    by_claim = {}
    for issue, record in (state.get("issues") or {}).items():
        if not isinstance(record, dict):
            continue
        roles = [("architect", record.get("architect"))]
        roles += list((record.get("workers") or {}).items())
        for role, entry in roles:
            held = claim_of(entry)
            session = held.get("session")
            if not session:
                continue
            token = (held.get("locator") or {}).get("claim")
            if token:
                by_claim[token] = session
            by_claim.setdefault(f"legion-{project}-{issue.lower()}-{role}", session)
    controller = (state.get("controllerLocator") or {}).get("sessionId")
    if controller:
        by_claim.setdefault(f"legion-{project}-controller", controller)
    return by_claim


def read_wakes(stream, since, until):
    """Every wake line of the log in time order. A line that is not JSON, carries another message
    or no readable time, or falls outside --since/--until, is skipped."""
    wakes = []
    for line in stream:
        if not line.strip():
            continue
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if not isinstance(entry, dict) or entry.get("msg") != MESSAGE:
            continue
        try:
            at = parse_time(str(entry.get("time", "")))
        except ValueError:
            continue
        if (since and at < since) or (until and at >= until):
            continue
        wakes.append({
            "at": at,
            "claim": entry.get("claim") or "",
            "kind": entry.get("kind") or "",
            "paced": entry.get("paced") is True,
            "rate": entry.get("rate"),
            "burst": entry.get("burst"),
        })
    wakes.sort(key=lambda wake: wake["at"])
    return wakes


def runs_of(wakes, gap):
    """The wakes cut into runs: a new run starts where the step from the wake before it is above
    gap. A run of at least --wave-size wakes is the wave the production check reads."""
    run = []
    for wake in wakes:
        if run and wake["at"] - run[-1]["at"] > gap:
            yield run
            run = []
        run.append(wake)
    if run:
        yield run


def counted(number, thing):
    """`1 wake` or `40 wakes`: an operator reads these lines, and `1 wakes` reads as a defect in
    the script rather than as the count it is."""
    return f"{number} {thing}" if number == 1 else f"{number} {thing}s"


def report_runs(wakes, gap, size):
    """The frame of the reading, on standard error: which runs the log holds, so the reader can
    say which wave the windows and the minutes cover."""
    for number, run in enumerate(runs_of(wakes, gap), start=1):
        where = f"{run[0]['at'].isoformat()} to {run[-1]['at'].isoformat()}"
        if len(run) >= size:
            print(f"wave {number}: {counted(len(run), 'wake')}, {where}", file=sys.stderr)
        else:
            print(f"run {number}: {counted(len(run), 'wake')}, under the {size} a wave holds, {where}",
                  file=sys.stderr)


def print_windows(wakes, by_claim, window):
    """One window per wake on standard output, and every claim the state does not name on standard
    error. Returns how many windows were printed and how many wakes were left out."""
    printed, unknown, first, last = 0, 0, None, None
    for wake in wakes:
        session = by_claim.get(wake["claim"])
        if not session:
            unknown += 1
            print(f"the state names no session for the claim {wake['claim'] or '(none)'}, "
                  f"woken at {wake['at'].isoformat()} ({wake['kind'] or 'no kind'}): no window for it",
                  file=sys.stderr)
            continue
        print(session, wake["at"].isoformat(), (wake["at"] + window).isoformat())
        printed += 1
        # The wakes are in time order, so the first window printed opens the span and the last
        # closes it: the two times to give gateway-uncached-by-organization.py.
        first = first or wake["at"]
        last = wake["at"] + window
    if printed:
        print(f"windows span {first.isoformat()} to {last.isoformat()}: "
              "the start and end to give gateway-uncached-by-organization.py", file=sys.stderr)
    return printed, unknown


def print_minutes(wakes):
    """The paced wakes of each minute on standard output, the highest minute, and the rate and
    burst the lines name. Returns how many lines were printed."""
    paced = [wake for wake in wakes if wake["paced"]]
    if not paced:
        return 0
    per_minute = collections.Counter(wake["at"].strftime("%Y-%m-%dT%H:%MZ") for wake in paced)
    for minute in sorted(per_minute):
        print(minute, per_minute[minute])
    # The earliest of the minutes that hold the most, so a reader who compares one minute with
    # the rate and burst is pointed at the first minute that reached it.
    highest, count = min(per_minute.items(), key=lambda item: (-item[1], item[0]))
    print("highest paced minute", highest, count)
    named = collections.Counter((wake["rate"], wake["burst"]) for wake in paced)
    for (rate, burst), lines in sorted(named.items(), key=lambda item: -item[1]):
        print(f"rate and burst the lines name: rate={rate} burst={burst} on {lines} lines")
    return len(per_minute) + 1 + len(named)


def main():
    parser = argparse.ArgumentParser(
        prog="wake-windows.py", description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("log", nargs="?", default="-",
                        help="the daemon's log, one JSON object per line; - (the default) is standard input")
    parser.add_argument("--state", metavar="STATE_JSON",
                        help="a saved body of GET /legion/v1/state, the claim-to-session mapping; "
                             "required for the windows and unread by --minutes")
    parser.add_argument("--window", type=float, default=10.0, metavar="MINUTES",
                        help="how long each session's window runs from its wake (default 10)")
    parser.add_argument("--minutes", action="store_true",
                        help="print the paced wakes of each minute in place of the windows")
    parser.add_argument("--since", metavar="TIME",
                        help="read no wake line before this ISO-8601 time, to cover one wave")
    parser.add_argument("--until", metavar="TIME",
                        help="read no wake line at or after this ISO-8601 time")
    parser.add_argument("--wave-size", type=int, default=40, metavar="N",
                        help="how many wakes a run holds to be called a wave (default 40)")
    parser.add_argument("--wave-gap", type=float, default=5.0, metavar="MINUTES",
                        help="the gap that ends a wave (default 5)")
    args = parser.parse_args()
    if not args.minutes and not args.state:
        parser.error("--state is required to build the windows: it maps each claim to its session")

    since = parse_time(args.since) if args.since else None
    until = parse_time(args.until) if args.until else None
    stream = sys.stdin if args.log == "-" else open(args.log)
    try:
        wakes = read_wakes(stream, since, until)
    finally:
        if stream is not sys.stdin:
            stream.close()

    bounded = "" if not (since or until) else " within the times given"
    if not wakes:
        print(f"no `{MESSAGE}` line in the log{bounded}: nothing to print", file=sys.stderr)
        return 1
    report_runs(wakes, datetime.timedelta(minutes=args.wave_gap), args.wave_size)
    print(f"{counted(len(wakes), 'wake line')} read, "
          f"{sum(1 for wake in wakes if wake['paced'])} of them paced", file=sys.stderr)

    if args.minutes:
        if print_minutes(wakes):
            return 0
        print(f"no paced wake among the {counted(len(wakes), 'line')} read: nothing to print. The "
              "pacing is off (rate 0), or every wake was an operator's request or the controller's "
              "own claim", file=sys.stderr)
        return 1
    by_claim = sessions_by_claim(json.load(open(args.state)))
    printed, unknown = print_windows(wakes, by_claim, datetime.timedelta(minutes=args.window))
    if not printed:
        print(f"no window from the {counted(len(wakes), 'wake line')} read: the state names a "
              "session for none of their claims", file=sys.stderr)
    return 1 if unknown or not printed else 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""analyze_restart.py NAME: the backlog-after-restart time of runs/NAME (restart-run.sh).

Reads runs/NAME/relay-frames.csv, runs/NAME.restart.json (kill, restart and first-answer times of
the listener) and runs/NAME.backlog.json (the envelopes backlogpub published while it was down).

A session's backlog is every first frame (not a duplicate) whose message the stream stored while
the listener was down (store time between the kill and the restarted listener's first answer), plus
every first frame stored before the kill and received after it (in flight at the kill). The
backlog time of a session is the receipt time of its last backlog frame minus the restart time (the
exec of the new listener) and minus its first answer; the result is the maximum and percentiles over
sessions. It also checks that every backlogpub envelope reached its session exactly once.
"""
import csv
import json
import os
import statistics
import sys

L = os.path.dirname(os.path.abspath(__file__))
name = sys.argv[1] if len(sys.argv) > 1 else "after-restart"
rs = json.load(open(os.path.join(L, "runs", name + ".restart.json")))
bl = json.load(open(os.path.join(L, "runs", name + ".backlog.json")))
kill, restart, answered = rs["kill_ns"], rs["restart_ns"], rs["answered_ns"]

last = {}
count = {}
backlog_ids = {}
dups = 0
redelivered_after = 0
for row in csv.DictReader(open(os.path.join(L, "runs", name, "relay-frames.csv"))):
    stored, recv, sess = int(row["stored_at_ns"]), int(row["recv_ns"]), row["session"]
    if row["duplicate"] == "true":
        dups += 1
        continue
    eid = int(row["event_id"])
    if bl["id_from"] <= eid <= bl["id_to"]:
        backlog_ids[eid] = backlog_ids.get(eid, 0) + 1
    in_outage = kill <= stored < answered
    in_flight = stored < kill and recv >= kill
    if in_outage or in_flight:
        count[sess] = count.get(sess, 0) + 1
        last[sess] = max(last.get(sess, 0), recv)

def ms(x):
    return x / 1e6

times = sorted(ms(t - restart) for t in last.values())
def pct(p):
    return times[int(p / 100 * (len(times) - 1))] if times else None

missing = [i for i in range(bl["id_from"], bl["id_to"] + 1) if backlog_ids.get(i, 0) == 0]
print(json.dumps({
    "listener_down_s": (restart - kill) / 1e9,
    "restart_to_first_answer_ms": ms(answered - restart),
    "sessions_with_backlog": len(last),
    "backlog_frames": sum(count.values()),
    "backlog_frames_per_session_median": statistics.median(count.values()) if count else None,
    "backlog_ms_after_restart_p50": pct(50),
    "backlog_ms_after_restart_p99": pct(99),
    "backlog_ms_after_restart_max (every session has its backlog)": times[-1] if times else None,
    "backlog_ms_after_first_answer_max": ms(max(last.values()) - answered) if last else None,
    "backlogpub_published": bl["published"],
    "backlogpub_publish_errors": bl["publish_errors"],
    "backlogpub_envelopes_received_once": sum(1 for v in backlog_ids.values() if v == 1),
    "backlogpub_envelopes_missing": len(missing),
    "duplicate_frames_in_run": dups,
}, indent=2))

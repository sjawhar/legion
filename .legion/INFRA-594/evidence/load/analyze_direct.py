#!/usr/bin/env python3
"""analyze_direct.py NAME: the delivery leg of a direct-run.sh AFTER run at 40 events/s.

Reads runs/NAME/relay-frames.csv and runs/NAME.directpub.json. Over the relay frames of directpub's
events (ids id_from..id_to): delivery latency from publish (receipt minus the stream's store time),
p50 / p99 / max, and the frames lost: 5 expected per published event (each issue has 5 watching
sessions at 1,000 sessions) minus the first frames received.
"""
import csv
import json
import os
import sys

L = os.path.dirname(os.path.abspath(__file__))
name = sys.argv[1]
dp = json.load(open(os.path.join(L, "runs", name + ".directpub.json")))
lat, per_id, dups = [], {}, 0
for row in csv.DictReader(open(os.path.join(L, "runs", name, "relay-frames.csv"))):
    eid = int(row["event_id"])
    if not dp["id_from"] <= eid <= dp["id_to"]:
        continue
    if row["duplicate"] == "true":
        dups += 1
        continue
    per_id[eid] = per_id.get(eid, 0) + 1
    lat.append((int(row["recv_ns"]) - int(row["stored_at_ns"])) / 1e6)
lat.sort()
pct = lambda p: lat[int(p / 100 * (len(lat) - 1))] if lat else None
expected = 5 * dp["published"]
print(json.dumps({
    "published": dp["published"], "publish_errors": dp["publish_errors"],
    "published_per_s": dp["published"] / ((dp["last_ns"] - dp["first_ns"]) / 1e9),
    "frames_expected": expected, "frames_received": len(lat), "frames_lost": expected - len(lat),
    "events_with_other_than_5_frames": sum(1 for i in range(dp["id_from"], dp["id_to"] + 1) if per_id.get(i, 0) != 5),
    "duplicates": dups,
    "from_publish_ms_p50": pct(50), "from_publish_ms_p99": pct(99), "from_publish_ms_max": lat[-1] if lat else None,
}, indent=2))

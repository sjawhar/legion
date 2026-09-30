"""Draw answered asks in a seeded random order, for writing the decision query set by hand.

The population is every answered question ask (not approvals) whose answer selects an option or
carries text. The order is random.Random(seed).shuffle over asks sorted by id, so the same seed
and export give the same order. The writer takes asks in this order, skipping only an ask whose
answer records no decision (a bare acknowledgement such as "Done"), and records each skip.

Usage: sample_decisions.py <data dir> <seed> <n to print> > sample.json
"""

import json
import os
import random
import sys

data, seed, n = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
titles = {}
with open(os.path.join(data, "issues.jsonl")) as f:
    for line in f:
        r = json.loads(line)
        titles[r["key"]] = r["title"]
asks = []
with open(os.path.join(data, "asks.jsonl")) as f:
    for line in f:
        a = json.loads(line)
        ans = a.get("answer") or {}
        if a["kind"] == "question" and a["state"] == "answered" and (ans.get("selected") or (ans.get("text") or "").strip()):
            asks.append(a)
asks.sort(key=lambda a: a["id"])
random.Random(seed).shuffle(asks)
out = [
    {
        "order": i,
        "ask_id": a["id"],
        "issue": a["issue_key"],
        "issue_title": titles.get(a["issue_key"] or ""),
        "question": a["question"],
        "options": [o["label"] + (": " + o["description"] if o.get("description") else "") for o in a.get("options") or []],
        "answer": a["answer"],
    }
    for i, a in enumerate(asks[:n])
]
json.dump({"population": len(asks), "seed": seed, "asks": out}, sys.stdout, indent=1)

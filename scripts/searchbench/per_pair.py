#!/usr/bin/env python3
"""One row per duplicate pair: keys, times, statuses, source versions, query texts, disposition,
and ranks under chosen systems. Reads <data>/asof.jsonl, <data>/issues.jsonl and a bench.py
result file.

Usage: per_pair.py --data DATA --results FILE [--systems A B C:titan-embed-v2 ...]
"""

import argparse
import json
import os
import re

import bench
import corpus

TEMPLATE_LINE = re.compile(r"^\s*(#.*|\*.*\*|None: this records what was agreed\.)?\s*$")


def empty_template(spec: str) -> bool:
    return all(TEMPLATE_LINE.match(line) for line in (spec or "").splitlines())


def clip(text: str, n: int = 200) -> str:
    t = " ".join(text.split())
    return (t[:n] + "…") if len(t) > n else t


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", required=True)
    ap.add_argument("--results", required=True)
    ap.add_argument("--systems", nargs="+", default=["A", "B", "C:titan-embed-v2"])
    args = ap.parse_args()
    items, _ = corpus.load(args.data)
    issues = {json.loads(line)["key"]: json.loads(line) for line in open(os.path.join(args.data, "issues.jsonl"))}
    pairs = [json.loads(line) for line in open(os.path.join(args.data, "asof.jsonl"))]
    queries = {q["id"]: q for q in bench.dup_queries(args.data, items)}
    res = json.load(open(args.results))["queries"]

    print("| # | Duplicate | Created | Status at filing / now | Spec version used | Survivor | Created | Status at filing / now | Spec version used | Disposition |")
    print("|---|---|---|---|---|---|---|---|---|---|")
    for n, p in enumerate(pairs, 1):
        d, s = issues[p["duplicate"]], issues[p["key"]]
        after = p["title"] is None
        if after:
            disp, s_status, s_ver = "not findable: survivor created after the duplicate", "did not exist", "n/a"
        else:
            s_status = f"{p['status']} / {s['status']}"
            s_ver = f"v{p['spec_version']} of v{s['spec_version']}" + (" (as of filing)" if p["survivor_changed"] else " (unchanged since)")
            if empty_template(p["spec"]):
                informative = not re.fullmatch(r"[\w.-]+/[\w.-]+#\d+", p["title"].strip())
                disp = "findable by title only: empty spec template" if informative else "not findable by content: empty spec template, title is a GitHub link"
            else:
                disp = "findable"
        print(f"| {n} | {d['key']} | {d['created_at'][:16]}Z | {p['dup_status']} / {d['status']} | v{p['dup_spec_version']} (first) | "
              f"{s['key']} | {s['created_at'][:16]}Z | {s_status} | {s_ver} | {disp} |")

    names = {"A": "Today's search", "B": "Fixed keyword", "C:titan-embed-v2": "Titan meaning-only = round-2 winner", "Q:titan-embed-v2+rerank-2.5": "Round-2 winner"}
    cols = " | ".join(f"{names.get(sy, sy)} ({mode})" for mode in ("title", "spec") for sy in args.systems)
    print(f"\n| # | Pair | {cols} |")
    print("|---|---|" + "---|" * (2 * len(args.systems)))
    for n, p in enumerate(pairs, 1):
        cells = []
        for mode in ("title", "spec"):
            sysmap = res[f"{p['duplicate']}:{mode}"]["systems"]
            cells += [str(sysmap[sy]["rank"] or "-") for sy in args.systems]
        print(f"| {n} | {p['duplicate']} → {p['key']} | " + " | ".join(cells) + " |")

    print("\n| # | Title search query (first 200 characters) | Spec search query (first 200 characters) | Sentences naming the survivor removed |")
    print("|---|---|---|---|")
    for n, p in enumerate(pairs, 1):
        qt, qs = queries[f"{p['duplicate']}:title"], queries[f"{p['duplicate']}:spec"]
        print(f"| {n} | {clip(qt['text'])} | {clip(qs['text'])} | {'yes' if qs['leak_removed'] else 'no'} |")


if __name__ == "__main__":
    main()

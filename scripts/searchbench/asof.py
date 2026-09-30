#!/usr/bin/env python3
"""Fetch the duplicate benchmark, and the texts as they stood when each duplicate was filed (GET only).

A survivor edited after the duplicate appeared often absorbed the duplicate's title and text during
consolidation, and a duplicate's title was often rewritten when it was closed, which would make the
pair look easier to find than it was. So the benchmark queries with the duplicate's title and first
spec version as filed, against the survivor's title and spec as of that moment.

Writes <data>/../dup-v1.json and <data>/asof.jsonl, one record per pair:
{duplicate, dup_title, dup_spec, key, title, spec, spec_version, survivor_changed}; the survivor
fields are null when the survivor was created after the duplicate (outside the recall ceiling).

Usage: asof.py --data /tmp/searchbench/data [--benchmark LEGION-386/duplicate-benchmark-v0-json]
"""

import argparse
import json
import os

import dget


def events(key: str) -> list[dict]:
    out, after = [], 0
    while True:
        page = dget.get(f"/issues/{key}/events", {"after": after, "order": "asc", "limit": 200})
        if not page:
            return out
        out.extend(page)
        after = page[-1]["seq"]


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", required=True)
    ap.add_argument("--benchmark", default="LEGION-386/duplicate-benchmark-v0-json")
    args = ap.parse_args()
    os.umask(0o077)
    issue, slug = args.benchmark.split("/")
    meta = dget.get(f"/issues/{issue}/artifacts/{slug}")
    bench = dget.get(f"/artifacts/{meta['id']}/versions/{meta['versions'][-1]['number']}")
    with open(os.path.join(args.data, "..", "dup-v1.json"), "w") as f:
        json.dump(bench, f)

    created = {}
    with open(os.path.join(args.data, "issues.jsonl")) as f:
        for line in f:
            r = json.loads(line)
            created[r["key"]] = r
    out = []
    for p in bench["pairs"]:
        dup, key = created[p["duplicate"]], created[p["expected"]]
        dup_created = next(e for e in events(dup["key"]) if e["type"] == "issue.created")
        dup_versions = dget.get(f"/issues/{dup['key']}/artifacts/{dup['spec_slug']}")["versions"]
        dup_spec = dget.get(f"/issues/{dup['key']}/artifacts/{dup['spec_slug']}/versions/{dup_versions[0]['number']}")["markdown"]
        dp = dup_created["payload"]
        rec = {"duplicate": dup["key"], "dup_title": dp["title"], "dup_spec": dup_spec, "key": key["key"],
               "dup_parent": dp.get("parent"), "dup_components": (dp.get("components") or {}).get("ids") or [],
               "dup_status": dp.get("status"), "dup_spec_version": dup_versions[0]["number"],
               "title": None, "spec": None, "spec_version": None, "survivor_changed": None, "parent": None, "components": [],
               "status": None}
        if key["created_at"] < dup["created_at"]:
            cutoff = dup["created_at"]
            # Every event whose payload is the whole issue: a close can retitle it (replay.SNAPSHOT_EVENTS).
            whole = ("issue.created", "issue.updated", "issue.closed")
            snaps = [e for e in events(key["key"]) if e["type"] in whole and e["created_at"] < cutoff]
            title = snaps[-1]["payload"]["title"] if snaps else key["title"]
            spec_meta = dget.get(f"/issues/{key['key']}/artifacts/{key['spec_slug']}")
            versions = [v for v in spec_meta["versions"] if v["created_at"] < cutoff]
            version = versions[-1]["number"] if versions else None
            spec = dget.get(f"/issues/{key['key']}/artifacts/{key['spec_slug']}/versions/{version}")["markdown"] if version else ""
            changed = title != key["title"] or version != key["spec_version"]
            sp = snaps[-1]["payload"] if snaps else {}
            rec.update(title=title, spec=spec, spec_version=version, survivor_changed=changed,
                       parent=sp.get("parent"), components=(sp.get("components") or {}).get("ids") or [], status=sp.get("status"))
        out.append(rec)
        print(f"{dup['key']} -> {key['key']}: dup title changed since filing {rec['dup_title'] != dup['title']}, "
              f"dup spec v1 {len(dup_spec)} chars; survivor changed since {rec['survivor_changed']}")
    with open(os.path.join(args.data, "asof.jsonl"), "w") as f:
        for r in out:
            f.write(json.dumps(r) + "\n")


if __name__ == "__main__":
    main()

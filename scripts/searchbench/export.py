#!/usr/bin/env python3
"""Export issues, primary specs and asks from Dispatch with GET requests only.

Writes <out>/issues.jsonl and <out>/asks.jsonl (mode 0600, directory 0700). The text is internal
company data: keep it outside any repository and delete it when the benchmark is done.

Usage: export.py --out /tmp/searchbench/data --projects AGENTC LEGION OPS
"""

import argparse
import json
import os
from concurrent.futures import ThreadPoolExecutor

import dget


def issue_record(key: str) -> tuple[dict, list[dict]]:
    d = dget.get(f"/issues/{key}")
    spec_text, spec_version, spec_slug = None, None, None
    primary = next((a for a in d["artifacts"] if a.get("primary")), None)
    if primary and primary["kind"] == "doc":
        spec_slug = primary["slug"]
        t = dget.get(f"/issues/{key}/artifacts/{spec_slug}/text")
        spec_text, spec_version = t["markdown"], t["version"]
    asks = dget.get(f"/issues/{key}/asks")
    rec = {
        "key": key,
        "project": d["project"],
        "title": d["title"],
        "status": d["status"],
        "created_at": d["created_at"],
        "closed_at": d["closed_at"],
        "parent": d["parent"],
        "labels": d["labels"],
        "components": d["components"],
        "spec_slug": spec_slug,
        "spec_version": spec_version,
        "spec": spec_text,
    }
    return rec, asks


def doc_asks(project: str) -> list[dict]:
    out = []
    for a in dget.get(f"/projects/{project}/artifacts"):
        if a.get("issue_key"):
            continue  # issue documents' asks come with the issue
        out.extend(dget.get(f"/artifacts/{a['id']}/asks"))
    return out


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--projects", nargs="+", required=True)
    args = ap.parse_args()
    os.umask(0o077)
    os.makedirs(args.out, mode=0o700, exist_ok=True)
    os.chmod(args.out, 0o700)

    keys = [i["key"] for p in args.projects for i in dget.get("/issues", {"project": p})]
    print(f"{len(keys)} issues")
    with ThreadPoolExecutor(8) as ex:
        results = list(ex.map(issue_record, keys))
        doc_ask_lists = list(ex.map(doc_asks, args.projects))

    asks: dict[str, dict] = {}
    for _, issue_asks in results:
        for a in issue_asks:
            asks[a["id"]] = a
    for lst in doc_ask_lists:
        for a in lst:
            asks[a["id"]] = a

    with open(os.path.join(args.out, "issues.jsonl"), "w") as f:
        for rec, _ in results:
            f.write(json.dumps(rec) + "\n")
    with open(os.path.join(args.out, "asks.jsonl"), "w") as f:
        for a in asks.values():
            f.write(json.dumps(a) + "\n")
    print(f"{len(results)} issues, {len(asks)} asks written to {args.out}")


if __name__ == "__main__":
    main()

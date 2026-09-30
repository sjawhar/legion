#!/usr/bin/env python3
"""Historical replay of the duplicate benchmark: each pair searched against Dispatch as it stood
when the duplicate was filed.

A case is one pair of duplicate-benchmark-v0 version 1. Its corpus is every issue in AGENTC, LEGION
and OPS created before the duplicate, each with its title (from the latest whole-issue event before
that moment: issue.created, issue.updated or issue.closed) and its primary spec (the latest version
before it). The duplicate's title and spec version 1 are the queries, built by bench.query_texts. A
pair whose survivor was filed after its duplicate is unsupported: no search could have found it. A
pair the production record shows is not a duplicate at all is excluded, with the reason.

Two sides score every supported case against its own corpus:
  baseline   today's search code on that day's data: Dispatch's own GET /api/v1/search on a scratch
             server (packages/dispatch/e2e/run-server.sh, its own Postgres) loaded case by case
             through the API, in filing order, with the corpus's titles and specs. Before a query
             runs there, the same GET goes to production to learn whether production accepts its
             URL; a refused query is an error, never a miss.
  candidate  offline, on SEARCHBENCH_PG: C (meaning only; the recommended setup's duplicate
             search), B (fixed keyword), W<w> (C and B by weighted RRF), F (C's top 150 reranked)
             and H (W0.25's top 150 reranked).

Run order (DATA is internal company text: keep it outside any repository, mode 0700):
  replay.py export    --data DATA                 GET-only production reads; writes cases.json, snaps.jsonl, manifest.json
  replay.py load      --data DATA                 snapshots, chunks and case membership into SEARCHBENCH_PG
  replay.py embed     --data DATA --model M --budget-usd X   each distinct chunk text once, one call at a
                                                  time; stops cleanly after --max-seconds, and a rerun resumes
  replay.py candidate --data DATA --model M --reranker R --out FILE --budget-usd X
  replay.py serve     --data DATA --checkout DIR --database-url <empty database> --port PORT
      builds and runs the scratch server from DIR, a clean jj checkout of the commit production
      runs (its /healthz names it), and records that launch in DATA/scratch_server.json; then
  replay.py baseline  --data DATA --out FILE     against the server that record names
  replay.py report    --data DATA --baseline FILE --candidate FILE   markdown tables; writes results.json
Embeddings are cached by the SHA-256 of each chunk's text, so identical text (an unchanged issue in
another case, or an unchanged paragraph of a later spec version) is embedded once. The baseline's
word check and the candidate share SEARCHBENCH_PG. Every command appends its run (arguments, start,
end, outcome, load average) to DATA/../runs.jsonl; the embedding and rerank commands append their
provider usage to DATA/../usage.jsonl; report copies both, with the scratch server's launch record
and the production and scratch servers' /healthz readings, into results.json.
"""

import argparse
import hashlib
import json
import os
import re
import signal
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

import bench
import corpus
import dget
import providers as P

PROJECTS = ("AGENTC", "LEGION", "OPS")
# The two inputs, pinned: the version read is this one, and its bytes must hash to Dispatch's own
# SHA-256 for it.
BENCHMARK = ("LEGION-386", "duplicate-benchmark-v0-json", 1)
QUERY_MANIFEST = ("LEGION-386", "query-manifest-json", 1)
# The events whose payload is the whole issue (title, status, primary document): issue.created
# (api/issue_create.go) and every PATCH, which is issue.closed when it moves the status to done and
# issue.updated otherwise, a reopen included (api/issue_patch.go). One PATCH can retitle and close at
# once, so a title can change on an issue.closed event and nowhere else.
SNAPSHOT_EVENTS = ("issue.created", "issue.updated", "issue.closed")
# Benchmark pairs the production record shows are not duplicates: excluded from every denominator.
NOT_DUPLICATES = {
    "LEGION-6": "LEGION-6 was completed, not closed as a duplicate: PR #923 merged and the architect signed "
                "it off (comment at 2026-09-11T23:00:24Z) before it closed as done; the benchmark's closing "
                "note is a separate message (2026-09-11T22:43:57Z) folding a different tester finding into LEGION-10",
}

# The e2e server's fixed identities (packages/dispatch/e2e/run-server.sh): a trusted header login
# for writes, the shared agent bearer for searches, as an agent's search tool sends one.
SCRATCH_USER = "alice"
SCRATCH_TOKEN = "e2e-token"
# Production indexes a document with its ask blocks removed by this pattern (store migration 0019);
# the scratch load removes them the same way, so the document's search vector is production's.
PROD_ASK_BLOCK = re.compile(r":::ask\{.*?:::", re.S)
# Dispatch seeds a blank spec with its template, so a spec that had no text is loaded as a single
# zero-width space: not blank to the server, no lexemes to search.
EMPTY_SPEC = "\u200b"
PLACEHOLDER_TITLE = "qqzzplaceholder"  # holds a key number whose issue is not in the corpus yet


def ts(s: str) -> datetime:
    """Dispatch timestamps are UTC with a Z and up to nine fractional digits."""
    if not s.endswith("Z"):
        raise ValueError(f"unexpected timestamp {s!r}")
    head, _, frac = s[:-1].partition(".")
    return datetime.fromisoformat(f"{head}.{(frac + '000000')[:6]}+00:00")


def sha(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def write_json(path: str, obj) -> None:
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f)
    os.replace(tmp, path)


def cget(data: str, path: str, params: dict | None = None):
    """dget.get with every response kept under DATA/cache, so a rerun reads nothing twice. What
    this replay reads before a filing moment is append-only (events, settled versions)."""
    key = sha(json.dumps([path, params], sort_keys=True))
    f = os.path.join(data, "cache", key[:2], key + ".json")
    if os.path.exists(f):
        with open(f) as fh:
            return json.load(fh)
    value = dget.get(path, params)
    os.makedirs(os.path.dirname(f), exist_ok=True)
    write_json(f, value)
    return value


# ---------------------------------------------------------------------------------------------
# export: GET-only reads of production, and the as-of reconstruction


EVENTS_PAGE = 200


def issue_events(data: str, key: str) -> list[dict]:
    """The issue's events in order, all of them."""
    out, after = [], 0
    while True:
        page = cget(data, f"/issues/{key}/events", {"after": after, "order": "asc", "limit": EVENTS_PAGE})
        out.extend(page)
        if len(page) < EVENTS_PAGE:
            return out
        after = page[-1]["seq"]


def issue_history(data: str, key: str) -> dict:
    """The issue's creation time and every whole-issue snapshot, in order."""
    evs = issue_events(data, key)
    snaps = [
        {"type": e["type"], "id": e.get("id"), "seq": e["seq"], "at": e["created_at"],
         "title": e["payload"]["title"], "primary_artifact_id": e["payload"].get("primary_artifact_id")}
        for e in evs if e["type"] in SNAPSHOT_EVENTS
    ]
    created = next((e for e in evs if e["type"] == "issue.created"), None)
    if created is None or snaps[0]["type"] != "issue.created":
        raise SystemExit(f"{key}: its first whole-issue event is not issue.created: {snaps[:1]}")
    return {"key": key, "created_at": created["payload"]["created_at"], "snapshots": snaps}


def latest_version(data: str, artifact_id: str | None) -> int | None:
    if not artifact_id:
        return None
    return max((v["number"] for v in cget(data, f"/artifacts/{artifact_id}")["versions"]), default=None)


def spec_at(data: str, artifact_id: str | None, cutoff: datetime) -> dict | None:
    """The artifact's latest version created before the cutoff, with its markdown."""
    if not artifact_id:
        return None
    meta = cget(data, f"/artifacts/{artifact_id}")
    before = [v for v in meta["versions"] if ts(v["created_at"]) < cutoff]
    if not before:
        return None
    v = max(before, key=lambda v: v["number"])
    md = cget(data, f"/artifacts/{artifact_id}/versions/{v['number']}")["markdown"]
    return {"artifact_id": artifact_id, "version": v["number"], "created_at": v["created_at"],
            "latest_version": latest_version(data, artifact_id), "markdown": md}


def event_ref(s: dict) -> dict:
    return {k: s[k] for k in ("type", "id", "seq", "at")}


def issue_at(data: str, hist: dict, cutoff: datetime) -> dict:
    """An issue's title and primary spec as they stood just before the cutoff. `title_event` is the
    event the title is read from, the latest whole-issue event before the cutoff; `title_set_by` is
    the event that gave the issue that title, the first of the unbroken run of events carrying it."""
    snaps = [s for s in hist["snapshots"] if ts(s["at"]) < cutoff]
    if not snaps:
        # The issue.created row is written a moment after the issue's created_at; take it anyway.
        snaps = hist["snapshots"][:1]
    first = len(snaps) - 1
    while first > 0 and snaps[first - 1]["title"] == snaps[-1]["title"]:
        first -= 1
    s = snaps[-1]
    return {"title": s["title"], "title_event": event_ref(s), "title_set_by": event_ref(snaps[first]),
            "primary_artifact_id": s["primary_artifact_id"], "spec": spec_at(data, s["primary_artifact_id"], cutoff)}


def pinned_version(data: str, ref: tuple[str, str, int]) -> tuple[dict, dict]:
    """The pinned version of an issue's JSON artifact, checked against Dispatch's SHA-256 for it."""
    issue, slug, number = ref
    meta = cget(data, f"/issues/{issue}/artifacts/{slug}")
    version = next((v for v in meta["versions"] if v["number"] == number), None)
    if version is None:
        raise SystemExit(f"{issue}/{slug} has no version {number}")
    body = dget.get(f"/artifacts/{meta['id']}/versions/{number}", raw=True)
    if sha(body) != version["sha256"]:
        raise SystemExit(f"{issue}/{slug} v{number}: bytes hash to {sha(body)}, Dispatch records {version['sha256']}")
    pin = {"ref": f"{issue}/{slug}", "artifact_id": meta["id"], "version": number, "created_at": version["created_at"],
           "sha256": version["sha256"], "latest_version": max(v["number"] for v in meta["versions"])}
    return pin, json.loads(body)


def harness_sources() -> dict[str, str]:
    """SHA-256 of each harness file this run executes, so a result names the code that produced it."""
    here = os.path.dirname(os.path.abspath(__file__))
    out = {}
    for name in ("replay.py", "bench.py", "corpus.py", "dget.py", "providers.py"):
        with open(os.path.join(here, name), "rb") as f:
            out[name] = hashlib.sha256(f.read()).hexdigest()
    return out


def utcnow() -> str:
    return datetime.now(timezone.utc).isoformat()


def cmd_export(args):
    data = args.data
    os.umask(0o077)
    os.makedirs(data, mode=0o700, exist_ok=True)
    health_start, started = dget.health(), utcnow()
    bpin, benchmark = pinned_version(data, BENCHMARK)
    qpin, qmanifest = pinned_version(data, QUERY_MANIFEST)
    manifest_sha = {q["id"]: q["sha256"] for row in qmanifest["rows"] for q in row["queries"]}

    keys = [i["key"] for proj in PROJECTS for i in cget(data, "/issues", {"project": proj})]
    with ThreadPoolExecutor(args.threads) as ex:
        hist = {h["key"]: h for h in ex.map(lambda k: issue_history(data, k), keys)}
    cutoffs = {p["duplicate"]: ts(hist[p["duplicate"]]["created_at"]) for p in benchmark["pairs"]}
    print(f"{len(keys)} issues listed, {sum(ts(h['created_at']) < max(cutoffs.values()) for h in hist.values())} "
          f"created before the last filing")

    snaps: dict[str, dict] = {}
    cases = []
    for row, p in enumerate(benchmark["pairs"], 1):
        dup, survivor, cutoff = p["duplicate"], p["expected"], cutoffs[p["duplicate"]]
        created = hist[dup]["snapshots"][0]
        aid = created["primary_artifact_id"]
        dup_v1 = cget(data, f"/artifacts/{aid}/versions/1")["markdown"] if aid else None
        title_q, spec_q, leak = bench.query_texts(created["title"], dup_v1, survivor)
        queries = [
            {"id": f"{dup}:title", "set": "dup-title", "text": title_q},
            {"id": f"{dup}:spec", "set": "dup-spec", "text": spec_q, "leak_removed": leak},
        ]
        for q in queries:
            q["sha256"] = sha(q["text"])
            q["matches_query_manifest"] = manifest_sha.get(q["id"]) == q["sha256"]

        members = []
        for h in hist.values():
            if h["key"] == dup or ts(h["created_at"]) >= cutoff:
                continue
            at = issue_at(data, h, cutoff)
            spec = at["spec"]
            sid = f"{h['key']}:{sha(at['title'])[:10]}:" + (f"{spec['artifact_id'][:8]}v{spec['version']}" if spec else "none")
            if sid not in snaps:
                snaps[sid] = {"id": sid, "key": h["key"], "title": at["title"],
                              "spec": {k: v for k, v in spec.items() if k != "markdown"} if spec else None,
                              "markdown": spec["markdown"] if spec else None}
            now = h["snapshots"][-1]
            changed = (now["title"] != at["title"] or now["primary_artifact_id"] != at["primary_artifact_id"]
                       or latest_version(data, at["primary_artifact_id"]) != (spec["version"] if spec else None))
            members.append({"key": h["key"], "created_at": h["created_at"], "snap": sid, "title_event": at["title_event"],
                            "title_set_by": at["title_set_by"], "changed_after_filing": changed})

        if dup in NOT_DUPLICATES:
            status = "not a duplicate: " + NOT_DUPLICATES[dup]
        elif ts(hist[survivor]["created_at"]) >= cutoff:
            status = "unsupported: the survivor was filed after the duplicate"
        elif not members or not title_q.strip():
            status = "missing data: the as-of corpus or the query is empty"
        else:
            status = "supported"
        cases.append({"row": row, "duplicate": dup, "survivor": survivor, "cutoff": hist[dup]["created_at"],
                      "survivor_created_at": hist[survivor]["created_at"], "status": status,
                      "queries": queries, "corpus": members})
        print(f"{row:2} {dup} -> {survivor}: {status[:60]}; {len(members)} issues, "
              f"{sum(m['changed_after_filing'] for m in members)} changed since; queries match manifest "
              f"{[q['matches_query_manifest'] for q in queries]}")

    revisions = {"benchmark": bpin, "query_manifest": qpin, "projects": PROJECTS, "snapshot_events": SNAPSHOT_EVENTS,
                 "export": {"started_at": started, "finished_at": utcnow()},
                 "production_health": {"start": health_start, "end": dget.health()}, "harness": harness_sources()}
    with open(os.path.join(data, "snaps.jsonl"), "w") as f:
        for s in snaps.values():
            f.write(json.dumps(s) + "\n")
    write_json(os.path.join(data, "cases.json"), {"revisions": revisions, "cases": cases})
    write_json(os.path.join(data, "manifest.json"), manifest(revisions, cases, snaps))
    print(f"{len(snaps)} distinct issue snapshots, {sum(len(c['corpus']) for c in cases)} case members, "
          f"{len(cases)} cases")


def manifest(revisions: dict, cases: list[dict], snaps: dict[str, dict]) -> dict:
    """Each case's corpus by reference: keys, the events each title came from, spec versions and
    timestamps, and text hashes; no titles or bodies."""
    used = {m["snap"] for c in cases for m in c["corpus"]}
    return {
        "what": "The as-of corpus of every case of the duplicate benchmark's historical replay: for each pair, every "
                "issue in AGENTC, LEGION and OPS created before the duplicate, with the primary spec version it had "
                "then and, per case, the event its title was read from (title_event, the latest whole-issue event "
                "before the filing) and the event that gave it that title (title_set_by). changed_after_filing: its "
                "title, primary document or spec version at the export differs from the filing's. Titles and bodies "
                "are omitted; the hashes identify the text. revisions pins the inputs, the production server read "
                "and the harness code.",
        "revisions": revisions,
        "snapshots": {
            sid: {"key": s["key"], "title_sha256": sha(s["title"]),
                  "spec": ({**s["spec"], "markdown_sha256": sha(s["markdown"])} if s["spec"] else None)}
            for sid, s in snaps.items() if sid in used
        },
        "cases": [
            {k: c[k] for k in ("row", "duplicate", "survivor", "cutoff", "survivor_created_at", "status")}
            | {"queries": [{k: q[k] for k in q if k != "text"} for q in c["queries"]],
               "corpus_size": len(c["corpus"]),
               "changed_after_filing": sum(m["changed_after_filing"] for m in c["corpus"]),
               "survivor_snapshot": next((m["snap"] for m in c["corpus"] if m["key"] == c["survivor"]), None),
               "corpus": [{"key": m["key"], "created_at": m["created_at"], "snapshot": m["snap"],
                           "title_event": m["title_event"], "title_set_by": m["title_set_by"],
                           "changed_after_filing": m["changed_after_filing"]} for m in c["corpus"]]}
            for c in cases
        ],
    }


def load_cases(data: str) -> tuple[dict, dict[str, dict]]:
    with open(os.path.join(data, "cases.json")) as f:
        cases = json.load(f)
    snaps = {}
    with open(os.path.join(data, "snaps.jsonl")) as f:
        for line in f:
            s = json.loads(line)
            snaps[s["id"]] = s
    return cases, snaps


def supported(cases: dict) -> list[dict]:
    return sorted((c for c in cases["cases"] if c["status"] == "supported"), key=lambda c: ts(c["cutoff"]))


# ---------------------------------------------------------------------------------------------
# candidate: offline, on SEARCHBENCH_PG


def emb_table(model: str) -> str:
    return "rp_" + bench.slug(model)


def cmd_load(args):
    cases, snaps = load_cases(args.data)
    conn = bench.connect()
    conn.execute(
        """create table if not exists rp_text (hash text primary key, text text not null);
           drop table if exists rp_case, rp_chunk, rp_snap;
           create table rp_snap (id text primary key, issue_key text not null, title text not null, kw tsvector not null);
           create table rp_chunk (snap_id text not null references rp_snap, ord int not null,
                                  hash text not null references rp_text, primary key (snap_id, ord));
           create table rp_case (case_id text not null, snap_id text not null references rp_snap, primary key (case_id, snap_id));"""
    )
    texts, chunks = {}, []
    with conn.cursor() as cur:
        rows = []
        for s in snaps.values():
            spec = corpus.strip_ask_blocks(s["markdown"] or "")
            rows.append((s["id"], s["key"], s["title"], s["title"], spec))
            for n, text in enumerate(corpus.chunk_markdown(s["title"], spec)):
                h = sha(text)
                texts[h] = text
                chunks.append((s["id"], n, h))
        cur.executemany(
            "insert into rp_snap values (%s,%s,%s, setweight(to_tsvector('english', %s), 'A') || setweight(to_tsvector('english', %s), 'D'))",
            rows,
        )
        cur.executemany("insert into rp_text values (%s,%s) on conflict do nothing", list(texts.items()))
        cur.executemany("insert into rp_chunk values (%s,%s,%s)", chunks)
        cur.executemany("insert into rp_case values (%s,%s)",
                        [(c["duplicate"], m["snap"]) for c in cases["cases"] for m in c["corpus"]])
    conn.execute("create index on rp_snap using gin (kw); create index on rp_chunk (hash); analyze;")
    chars = sum(len(t) for t in texts.values())
    print(f"{len(snaps)} snapshots, {len(chunks)} chunks, {len(texts)} distinct chunk texts ({chars:,} characters)")


def process_spend() -> float:
    """What this process has spent so far at bench.PRICES list prices."""
    total = 0.0
    for prov, u in P.USAGE.items():
        unit, price, _ = bench.PRICES.get(prov, (None, 0.0, None))
        if unit:
            total += u.get(unit, 0.0) * price
    return total


def cmd_embed(args):
    emb = P.EMBEDDERS[args.model]()
    emb.concurrency = 1  # one request at a time: the devbox is shared
    conn = bench.connect()
    table = emb_table(args.model)
    conn.execute(f"create table if not exists {table} (hash text primary key references rp_text, v vector({emb.dim}))")
    todo = conn.execute(
        f"""select t.hash, t.text from rp_text t where exists (select 1 from rp_chunk k where k.hash = t.hash)
              and not exists (select 1 from {table} e where e.hash = t.hash) order by length(t.text)"""
    ).fetchall()
    print(f"{args.model}: {len(todo)} chunk texts to embed")
    t0 = time.time()
    try:
        for s in range(0, len(todo), args.step):
            part = todo[s : s + args.step]
            vecs = emb.embed([t for _, t in part], "document")
            with conn.cursor() as cur:
                cur.executemany(f"insert into {table} values (%s, %s)", [(h, v) for (h, _), v in zip(part, vecs)])
            done = s + len(part)
            spent = process_spend()
            print(f"  {done}/{len(todo)}  {done / (time.time() - t0):.1f}/s  ${spent:.3f}", flush=True)
            if spent > args.budget_usd:
                raise SystemExit(f"stopped: this process has spent ${spent:.2f}, over the ${args.budget_usd:.2f} budget")
            if time.time() - t0 > args.max_seconds:
                print(f"stopping after {args.max_seconds}s with {len(todo) - done} left; rerun to resume")
                break
    finally:
        bench.log_usage(args.data, f"replay embed {args.model}")


def rp_dense(conn, model: str, case: str, v, n: int = bench.FUSE_DEPTH) -> tuple[list[str], dict[str, float]]:
    rows = conn.execute(
        f"""select s.issue_key, max(1 - (e.v <=> %(v)s)) as sim
              from rp_case c join rp_snap s on s.id = c.snap_id
              join rp_chunk k on k.snap_id = c.snap_id join {emb_table(model)} e on e.hash = k.hash
             where c.case_id = %(case)s group by s.issue_key order by sim desc, s.issue_key limit %(n)s""",
        {"v": v, "case": case, "n": n},
    ).fetchall()
    return [r[0] for r in rows], {r[0]: r[1] for r in rows}


def rp_keyword(conn, case: str, text: str, n: int = bench.FUSE_DEPTH) -> list[str]:
    rows = conn.execute(
        f"""with q as (select {bench.KW_TSQUERY} as tsq)
            select s.issue_key from rp_case c join rp_snap s on s.id = c.snap_id, q
             where c.case_id = %(case)s and s.kw @@ q.tsq
             order by ts_rank_cd(s.kw, q.tsq, 1) desc, s.issue_key limit %(n)s""",
        {"q": bench.keyword_text(text), "case": case, "n": n},
    ).fetchall()
    return [r[0] for r in rows]


def rp_passages(conn, model: str, case: str, v, ids: list[str]) -> dict[str, str]:
    rows = conn.execute(
        f"""select s.issue_key, t.text, 1 - (e.v <=> %(v)s) as sim
              from rp_case c join rp_snap s on s.id = c.snap_id join rp_chunk k on k.snap_id = c.snap_id
              join rp_text t on t.hash = k.hash join {emb_table(model)} e on e.hash = k.hash
             where c.case_id = %(case)s and s.issue_key = any(%(ids)s)
             order by s.issue_key, sim desc, k.ord""",
        {"v": v, "case": case, "ids": ids},
    ).fetchall()
    return bench.passage_docs(rows)


def cmd_candidate(args):
    cases, _ = load_cases(args.data)
    conn = bench.connect()
    emb = P.EMBEDDERS[args.model]()
    emb.concurrency = 1
    rr = P.RERANKERS[args.reranker]()
    m = args.model
    results = {"model": m, "reranker": args.reranker, "rerank_depth": bench.RERANK_DEPTH, "depth": bench.DEPTH, "queries": {}}
    try:
        for c in supported(cases):
            corpus_keys = {x["key"] for x in c["corpus"]}
            for q in c["queries"]:
                v, q_dt = bench.query_vector(conn, emb, q["text"])
                dense, _ = rp_dense(conn, m, c["duplicate"], v)
                kw = rp_keyword(conn, c["duplicate"], q["text"])
                lists = {f"C:{m}": dense, "B": kw}
                for w in (0.25, 0.5, 0.75):
                    lists[f"W{w}:{m}"] = bench.rrf_weighted(dense, kw, w)
                depth = bench.RERANK_DEPTH
                for name, base in ((f"F:{m}+{args.reranker}", dense), (f"H:{m}+{args.reranker}", lists[f"W0.25:{m}"])):
                    top = base[:depth]
                    docs = rp_passages(conn, m, c["duplicate"], v, top)
                    reranked, _ = bench.rerank(conn, rr, q["text"], top, docs)
                    lists[name] = reranked + base[depth:]
                systems = {name: {"rank": bench.rank_of(lst, c["survivor"]), "top": lst[:10], "returned": len(lst)}
                           for name, lst in lists.items()}
                results["queries"][q["id"]] = {"case": c["duplicate"], "set": q["set"], "expected": c["survivor"],
                                               "findable": c["survivor"] in corpus_keys, "candidates": len(corpus_keys),
                                               "systems": systems}
                print(q["id"], {k: s["rank"] for k, s in systems.items()}, f"${process_spend():.3f}", flush=True)
                write_json(args.out, results)
                if process_spend() > args.budget_usd:
                    raise SystemExit(f"stopped: this process has spent ${process_spend():.2f}, over the ${args.budget_usd:.2f} budget")
    finally:
        bench.log_usage(args.data, f"replay candidate {os.path.basename(args.out)}")


# ---------------------------------------------------------------------------------------------
# baseline: Dispatch's own search on a scratch server


class Scratch:
    """A local Dispatch this command writes to. Loopback only, so no write can reach a real one."""

    def __init__(self, url: str):
        host = urllib.parse.urlparse(url).hostname
        if host not in ("127.0.0.1", "localhost", "::1"):
            raise SystemExit(f"refusing {url}: the baseline writes to its server, which must be a local scratch server")
        self.root = url.rstrip("/")
        self.base = self.root + "/api/v1"

    def health(self) -> dict:
        """The scratch server's /healthz: its schema version, and the build commit it reports. That
        commit is only the string its build was stamped with (-ldflags main.buildCommit); `serve`
        stamps it from the checkout's revision, so the launch record, not this reading, is the
        build's identity."""
        at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        with urllib.request.urlopen(self.root + "/healthz", timeout=30) as resp:
            return {"read_at": at, **json.loads(resp.read())}

    def call(self, method: str, path: str, body=None, params=None, human: bool = True):
        url = self.base + path + ("?" + urllib.parse.urlencode(params) if params else "")
        headers = {"Content-Type": "application/json"}
        headers.update({"X-Dispatch-User": SCRATCH_USER} if human else {"Authorization": f"Bearer {SCRATCH_TOKEN}"})
        req = urllib.request.Request(url, method=method, headers=headers,
                                     data=json.dumps(body).encode() if body is not None else None)
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                return json.loads(resp.read() or b"null")
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"{method} {path} -> {e.code}: {e.read().decode()[:400]}") from e


def scratch_spec(markdown: str | None) -> str:
    text = PROD_ASK_BLOCK.sub("", markdown or "")
    return text if text.strip() else EMPTY_SPEC


def production_status(text: str) -> dict:
    """Whether production accepts this query's GET /api/v1/search URL. Only the status is kept."""
    try:
        res = dget.get("/search", {"q": text, "limit": 50})
        return {"status": 200, "returned": len(res["results"])}
    except RuntimeError as e:
        m = re.search(r" -> (\d{3}): (.*)", str(e), re.S)
        if not m:
            raise
        return {"status": int(m.group(1)), "error": m.group(2)[:200]}


SCRATCH_RECORD = "scratch_server.json"


def checkout_revision(checkout: str) -> dict:
    """The revision a scratch server is built from, read with jj in its checkout (a jj workspace,
    named by any directory in it). The read snapshots the working copy first, so an edit on disk
    shows as a working-copy commit with changes, and that is refused: the server would not be the
    revision it names. A path jj's ignore rules exclude is outside this check."""
    def jj(*a: str) -> str:
        return subprocess.run(["jj", "--no-pager", "--color=never", *a], cwd=checkout, check=True,
                              capture_output=True, text=True).stdout

    root = jj("workspace", "root").strip()
    wc, state, parents = jj("log", "--no-graph", "-r", "@", "-T",
                            'commit_id ++ "\\t" ++ if(empty, "clean", "changed") ++ "\\t" '
                            '++ parents.map(|p| p.commit_id()).join(",")').split("\t")
    if state != "clean":
        raise SystemExit(f"{root}: the working copy holds changes, so a server built from it is not a revision:\n"
                         + jj("diff", "--summary", "-r", "@"))
    if "," in parents:
        raise SystemExit(f"{root}: the working-copy commit is a merge ({parents}); check out one revision")
    return {"checkout": root, "revision": parents, "working_copy_commit": wc, "working_copy": state,
            "read_at": utcnow()}


def redact_url(url: str) -> str:
    p = urllib.parse.urlsplit(url)
    if p.password is None:
        return url
    host = p.hostname + (f":{p.port}" if p.port else "")
    return urllib.parse.urlunsplit(p._replace(netloc=f"{p.username}:***@{host}"))


def cmd_serve(args):
    """Build and run the scratch server from a clean checkout, in the foreground until it exits, and
    keep DATA/scratch_server.json as the record of that launch: the command and the environment it
    set, the checkout, revision and working-copy state it was built from, the Go toolchain
    run-server.sh resolves there, the server's pid, its /healthz once it answers, and its exit."""
    path = os.path.join(args.data, SCRATCH_RECORD)
    url = f"http://127.0.0.1:{args.port}"
    s = Scratch(url)
    try:
        held = s.health()
    except (urllib.error.URLError, ConnectionError, TimeoutError):
        held = None
    if held is not None:
        raise SystemExit(f"{url} already answers /healthz ({held}): another server holds the port")
    source = checkout_revision(args.checkout)
    rev, cwd = source["revision"], source["checkout"]

    def run(*a: str) -> str:
        return subprocess.run(list(a), cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()

    goroot = run("go", "env", "GOROOT")  # the Go binary run-server.sh runs: $(go env GOROOT)/bin/go
    argv = ["nice", "-n", "19", "bash", "packages/dispatch/e2e/run-server.sh"]
    env_set = {"DATABASE_URL": args.database_url, "DISPATCH_E2E_PORT": str(args.port),
               "GOFLAGS": f"-p=1 -ldflags=-X=main.buildCommit={rev}"}
    env_unset = ["CI"]
    rec = {"what": "The scratch Dispatch server's launch, written by replay.py serve: the command it ran from the "
                   "checkout (argv, cwd, the environment it set and unset), the revision and working-copy state it "
                   "was built from, the Go toolchain, the pid, /healthz once it answered, and its exit. The build "
                   "commit /healthz reports is stamped from source.revision by the GOFLAGS here.",
           "url": url, "argv": argv, "cwd": cwd,
           "env_set": {**env_set, "DATABASE_URL": redact_url(args.database_url)}, "env_unset": env_unset,
           "source": source, "go": {"goroot": goroot, "version": run(os.path.join(goroot, "bin", "go"), "version")},
           "started_at": utcnow()}
    env = {k: v for k, v in os.environ.items() if k not in env_unset} | env_set
    proc = subprocess.Popen(argv, cwd=cwd, env=env, start_new_session=True)
    rec["pid"] = proc.pid
    write_json(path, rec)

    def stop(*_):
        raise SystemExit("serve: terminated")

    signal.signal(signal.SIGTERM, stop)
    try:
        deadline = time.time() + args.ready_seconds
        while "health_at_ready" not in rec:
            if proc.poll() is not None:
                raise SystemExit(f"the scratch server exited ({proc.returncode}) before it answered /healthz")
            try:
                rec["health_at_ready"] = s.health()
            except (urllib.error.URLError, ConnectionError, TimeoutError):
                if time.time() > deadline:
                    raise SystemExit(f"the scratch server did not answer /healthz within {args.ready_seconds}s")
                time.sleep(2)
        if rec["health_at_ready"].get("commit") != rev:
            raise SystemExit(f"{url} reports commit {rec['health_at_ready'].get('commit')!r}, not {rev}")
        write_json(path, rec)
        print(f"scratch server ready at {url}, pid {proc.pid}: built from {rev} in {cwd} (clean)", flush=True)
        proc.wait()
    finally:
        if proc.poll() is None:
            os.killpg(proc.pid, signal.SIGTERM)
            proc.wait()
        rec["exit"] = {"at": utcnow(), "code": proc.returncode}
        write_json(path, rec)


def scratch_record(data: str) -> dict:
    """The launch record of the scratch server serve is running now."""
    path = os.path.join(data, SCRATCH_RECORD)
    if not os.path.exists(path):
        raise SystemExit(f"no {path}: start the scratch server with `replay.py serve`, which records the build it runs")
    with open(path) as f:
        rec = json.load(f)
    if "health_at_ready" not in rec or "exit" in rec:
        raise SystemExit(f"{path}: the server it records (started {rec['started_at']}) is not running")
    try:
        os.kill(rec["pid"], 0)
    except ProcessLookupError:
        raise SystemExit(f"{path}: its server's pid {rec['pid']} is gone; the record was left by a serve that was killed")
    return rec


def cmd_baseline(args):
    cases, snaps = load_cases(args.data)
    server = scratch_record(args.data)
    s = Scratch(server["url"])
    if s.health().get("commit") != server["source"]["revision"]:
        raise SystemExit(f"{server['url']} does not report the revision its launch record names: another server holds the port")
    have = {p["key"] for p in s.call("GET", "/projects")}
    for proj in PROJECTS:
        if proj not in have:
            s.call("POST", "/projects", {"key": proj, "name": proj})
    loaded: dict[str, str] = {}  # key -> snapshot id, or PLACEHOLDER_TITLE
    top: dict[str, int] = {proj: 0 for proj in PROJECTS}
    for proj in PROJECTS:
        if s.call("GET", "/issues", params={"project": proj}):
            raise SystemExit(f"the scratch server already holds {proj} issues: start it on an empty database")
    results = {"server": "scratch Dispatch (e2e run-server.sh)", "scratch_server": server, "queries": {}, "loads": [],
               "health": {"start": {"scratch": s.health(), "production": dget.health()}}}

    for c in supported(cases):
        t0 = time.time()
        target = {m["key"]: m["snap"] for m in c["corpus"]}
        created = updated_title = updated_spec = placeholders = 0
        for proj in PROJECTS:
            want = max((int(k.split("-")[1]) for k in target if k.startswith(proj + "-")), default=0)
            for n in range(top[proj] + 1, want + 1):
                key = f"{proj}-{n}"
                sid = target.get(key)
                snap = snaps[sid] if sid else None
                body = {"project": proj, "force": True,
                        "title": snap["title"] if snap else PLACEHOLDER_TITLE,
                        "spec": scratch_spec(snap["markdown"]) if snap else EMPTY_SPEC}
                got = s.call("POST", "/issues", body)
                if got["key"] != key:
                    raise SystemExit(f"scratch server assigned {got['key']} where {key} was expected")
                loaded[key] = sid or PLACEHOLDER_TITLE
                created += 1
                placeholders += sid is None
            top[proj] = max(top[proj], want)
        for key, sid in target.items():
            old = loaded[key]
            if old == sid:
                continue
            new = snaps[sid]
            prev = snaps.get(old)
            if prev is None or prev["title"] != new["title"]:
                s.call("PATCH", f"/issues/{key}", {"title": new["title"]})
                updated_title += 1
            if prev is None or scratch_spec(prev["markdown"]) != scratch_spec(new["markdown"]):
                s.call("POST", f"/issues/{key}/artifacts", {"name": "spec.md", "content": scratch_spec(new["markdown"])})
                updated_spec += 1
            loaded[key] = sid
        stray = [k for k, v in loaded.items() if v != PLACEHOLDER_TITLE and k not in target]
        if stray:
            raise SystemExit(f"{c['duplicate']}: loaded issues outside the corpus: {stray[:5]}")
        check = verify_scratch(s, target, snaps, c["survivor"])
        # Positive control: the survivor's own as-of title must find it, or a miss below proves nothing.
        control_title = snaps[target[c["survivor"]]]["title"]
        control = bench.collapse_hits(s.call("GET", "/search", params={"q": control_title, "limit": 50}, human=False)["results"], set(target))
        check["control_rank"] = bench.rank_of(control, c["survivor"])
        results["loads"].append({"case": c["duplicate"], "created": created, "placeholders": placeholders,
                                 "titles_updated": updated_title, "specs_updated": updated_spec,
                                 "corpus": len(target), "verify": check, "seconds": round(time.time() - t0, 1)})
        print(f"{c['duplicate']}: loaded {len(target)} issues (+{created} created, {placeholders} placeholders, "
              f"{updated_title} titles and {updated_spec} specs updated); survivor's own title ranks it "
              f"{check['control_rank']}; {check['text_differs']} stored specs re-serialised, "
              f"{len(check['words_differ'])} with different words", flush=True)

        for q in c["queries"]:
            prod = production_status(q["text"])
            t = time.time()
            res = s.call("GET", "/search", params={"q": q["text"], "limit": 50}, human=False)["results"]
            dt = time.time() - t
            ranked = bench.collapse_hits(res, set(target))
            outside = sum(1 for r in res if r.get("issue") and r["issue"]["key"] not in target)
            rec = {"case": c["duplicate"], "set": q["set"], "expected": c["survivor"], "findable": c["survivor"] in target,
                   "candidates": len(target), "production_url": prod, "latency": dt, "hits": len(res),
                   "hits_outside_corpus": outside, "top": ranked[:10], "returned": len(ranked)}
            if prod["status"] == 200:
                rec["rank"] = bench.rank_of(ranked, c["survivor"])
            else:
                rec["rank"] = None
                rec["error"] = f"production refuses this query's URL ({prod['status']}): {prod.get('error', '')}"
                rec["scratch_rank_not_counted"] = bench.rank_of(ranked, c["survivor"])
            results["queries"][q["id"]] = rec
            print(" ", q["id"], "rank", rec["rank"], "error" if "error" in rec else "", flush=True)
        write_json(args.out, results)
    results["health"]["end"] = {"scratch": s.health(), "production": dget.health()}
    write_json(args.out, results)


LEXEMES = "select array(select unnest(tsvector_to_array(to_tsvector('english', %s))) order by 1)"


def verify_scratch(s: Scratch, target: dict[str, str], snaps: dict[str, dict], survivor: str) -> dict:
    """Read back every corpus issue: its title must equal the as-of title. Dispatch re-serialises an
    uploaded document (list numbering, emphasis markers; and a `|` inside inline code in a table row
    ends that cell, dropping the rest of the row), so a spec whose stored text differs is compared
    by the words its search index holds: the to_tsvector lexemes of the stored text against those
    of the as-of text."""
    conn = bench.connect()
    titles = {}
    for proj in PROJECTS:
        for i in s.call("GET", "/issues", params={"project": proj}):
            titles[i["key"]] = i["title"]
    out = {"titles_differ": [], "text_differs": 0, "words_differ": {}}
    for key, sid in target.items():
        snap = snaps[sid]
        if titles.get(key) != snap["title"].strip():
            out["titles_differ"].append(key)
        want = scratch_spec(snap["markdown"])
        got = s.call("GET", f"/issues/{key}/artifacts/spec/text")["markdown"]
        if got.strip() == want.strip():
            continue
        out["text_differs"] += 1
        a, b = (set(conn.execute(LEXEMES, (t,)).fetchone()[0]) for t in (want, got))
        if a != b:
            out["words_differ"][key] = {"missing": sorted(a - b)[:10], "added": sorted(b - a)[:10]}
    out["survivor_words_differ"] = survivor in out["words_differ"]
    return out


# ---------------------------------------------------------------------------------------------
# report


def cmd_report(args):
    cases, snaps = load_cases(args.data)
    baseline = json.load(open(args.baseline))
    cand = json.load(open(args.candidate))
    base = baseline["queries"]
    m, r = cand["model"], cand["reranker"]
    table = [("Today's search", "A"), ("Meaning only", f"C:{m}"), ("Meaning + rerank", f"F:{m}+{r}"),
             ("Quarter-weight merge", f"W0.25:{m}"), ("Quarter-weight merge + rerank", f"H:{m}+{r}")]
    summary_systems = table + [("Half-weight merge", f"W0.5:{m}"), ("Three-quarter-weight merge", f"W0.75:{m}"),
                               ("Fixed keyword search", "B")]

    def outcome(qid: str, name: str, case: dict) -> tuple[str, int | None]:
        """('rank'|'miss'|'error'|'unsupported'|'not a duplicate'|'missing data', rank)."""
        if case["status"] != "supported":
            return case["status"].split(":")[0], None
        if name == "A":
            b = base[qid]
            if "error" in b:
                return "error", None
            rank = b["rank"]
        else:
            rank = cand["queries"][qid]["systems"][name]["rank"]
        return ("rank" if rank else "miss"), rank

    def cell(qid: str, name: str, case: dict) -> str:
        kind, rank = outcome(qid, name, case)
        if kind == "error":
            return f"error ({base[qid]['production_url']['status']})"
        return {"rank": str(rank), "miss": "-"}.get(kind, kind)

    for mode, label in (("title", "Title search"), ("spec", "Whole-spec search")):
        print(f"\n**{label}.** The survivor's rank among the issues that existed when the duplicate was filed. "
              "\"-\": not in today's search's 50 results, or not in a candidate's top 150.\n")
        print("| # | Pair | Filed | Issues then | " + " | ".join(n for n, _ in table) + " |")
        print("| --- | --- | --- | --- | " + " | ".join("---" for _ in table) + " |")
        for c in cases["cases"]:
            qid = f"{c['duplicate']}:{mode}"
            size = len(c["corpus"]) if c["status"] == "supported" else "-"
            print(f"| {c['row']} | {c['duplicate']} → {c['survivor']} | {c['cutoff'][:16]}Z | {size} | "
                  + " | ".join(cell(qid, s, c) for _, s in table) + " |")

    sup = [c for c in cases["cases"] if c["status"] == "supported"]
    print(f"\n**Summary over the {len(sup)} supported pairs.** Top-1, top-5 and top-10 counts; MRR counts an error or a miss as 0.\n")
    print("| System | Title: top 1 / 5 / 10 | Title MRR | Spec: top 1 / 5 / 10 | Spec MRR | Errors |")
    print("| --- | --- | --- | --- | --- | --- |")
    summary = {}
    for label, name in summary_systems:
        row, errors = [label], 0
        summary[name] = {}
        for mode in ("title", "spec"):
            ranks = []
            for c in sup:
                kind, rank = outcome(f"{c['duplicate']}:{mode}", name, c)
                errors += kind == "error"
                ranks.append(rank)
            n = len(ranks)
            top = [sum(1 for x in ranks if x and x <= k) for k in (1, 5, 10)]
            mrr = sum(1 / x for x in ranks if x) / n
            summary[name][mode] = {"n": n, "top1": top[0], "top5": top[1], "top10": top[2], "mrr": round(mrr, 3)}
            row += [" / ".join(map(str, top)), f"{mrr:.3f}"]
        summary[name]["errors"] = errors
        print("| " + " | ".join(row) + f" | {errors} |")

    def jsonl(name: str) -> list[dict]:
        path = os.path.join(args.data, "..", name)
        return [json.loads(line) for line in open(path)] if os.path.exists(path) else []

    out = {"what": "Per-case results of the duplicate benchmark's historical replay: for each pair, today's search on a "
                   "scratch Dispatch holding the issues as they stood when the duplicate was filed, and the candidate "
                   "setups on the same corpus. Ranks count from 1; null is a miss, an error, an unsupported case or a "
                   "pair that is not a duplicate, as the status and error fields say. revisions pins the inputs, the "
                   "production server the export read and the harness code; scratch_server is replay.py serve's "
                   "record of the scratch server's launch: the command, the checkout, revision and working-copy "
                   "state it was built from, and the Go toolchain; baseline_health is /healthz of the scratch and "
                   "production servers when the baseline started and ended (the scratch server's commit there is "
                   "the build stamp serve set from that revision, not evidence on its own); runs and usage are the "
                   "run and provider-usage ledgers of this replay.",
           "revisions": cases["revisions"], "scratch_server": baseline["scratch_server"],
           "baseline_health": baseline["health"],
           "systems": {name: label for label, name in summary_systems}, "candidate_model": m, "candidate_reranker": r,
           "summary": summary, "scratch_loads": baseline["loads"],
           "runs": jsonl("runs.jsonl"),
           "usage": {"ledger": jsonl("usage.jsonl"),
                     "priced": [{"provider": p, "usage": u, "usd": round(usd, 4), "note": note}
                                for p, u, usd, note in bench.spend(args.data)]},
           "cases": []}
    for c in cases["cases"]:
        rec = {k: c[k] for k in ("row", "duplicate", "survivor", "cutoff", "survivor_created_at", "status")}
        rec["corpus_size"] = len(c["corpus"])
        rec["changed_after_filing"] = sum(x["changed_after_filing"] for x in c["corpus"])
        sv = next((x for x in c["corpus"] if x["key"] == c["survivor"]), None)
        if sv:
            rec["survivor_as_of"] = {"snapshot": sv["snap"], "title_event": sv["title_event"],
                                     "title_set_by": sv["title_set_by"], "spec": snaps[sv["snap"]]["spec"]}
        rec["queries"] = {}
        for q in c["queries"]:
            qid = q["id"]
            rec["queries"][q["set"]] = {"id": qid, "sha256": q["sha256"], "matches_query_manifest": q["matches_query_manifest"],
                                        "outcomes": {name: dict(zip(("outcome", "rank"), outcome(qid, name, c)))
                                                     for _, name in summary_systems},
                                        "baseline": base.get(qid), "candidate": cand["queries"].get(qid)}
        out["cases"].append(rec)
    write_json(os.path.join(args.data, "results.json"), out)


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("export", "load", "embed", "candidate", "serve", "baseline", "report"):
        p = sub.add_parser(name)
        p.add_argument("--data", required=True)
        if name == "export":
            p.add_argument("--threads", type=int, default=4)
        if name in ("embed", "candidate"):
            p.add_argument("--model", default="titan-embed-v2")
            p.add_argument("--budget-usd", type=float, required=True)
        if name == "embed":
            p.add_argument("--step", type=int, default=50)
            p.add_argument("--max-seconds", type=int, default=3000, help="stop cleanly after this long; a rerun resumes")
        if name == "candidate":
            p.add_argument("--reranker", default="rerank-2.5")
            p.add_argument("--out", required=True)
        if name == "serve":
            p.add_argument("--checkout", required=True, help="a clean jj checkout of the commit production runs")
            p.add_argument("--database-url", required=True, help="an empty Postgres database for the scratch server")
            p.add_argument("--port", type=int, required=True)
            p.add_argument("--ready-seconds", type=int, default=1800, help="how long the build may take to answer /healthz")
        if name == "baseline":
            p.add_argument("--out", required=True)
        if name == "report":
            p.add_argument("--baseline", required=True)
            p.add_argument("--candidate", required=True)
    args = ap.parse_args()
    recorded = {k: redact_url(v) if k == "database_url" else v for k, v in vars(args).items() if k != "cmd"}
    run = {"cmd": args.cmd, "args": recorded, "started_at": utcnow(), "loadavg_start": os.getloadavg()}
    try:
        {"export": cmd_export, "load": cmd_load, "embed": cmd_embed, "candidate": cmd_candidate,
         "serve": cmd_serve, "baseline": cmd_baseline, "report": cmd_report}[args.cmd](args)
        run["outcome"] = "ok"
    except SystemExit as e:
        run["outcome"] = f"exit: {e.code}"
        raise
    except BaseException as e:
        run["outcome"] = f"{type(e).__name__}: {str(e)[:300]}"
        raise
    finally:
        run.update(finished_at=utcnow(), loadavg_end=os.getloadavg())
        with open(os.path.join(args.data, "..", "runs.jsonl"), "a") as f:
            f.write(json.dumps(run) + "\n")


if __name__ == "__main__":
    main()

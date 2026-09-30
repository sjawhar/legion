#!/usr/bin/env python3
"""Score Dispatch's search candidates against the measured query set, each search against Dispatch
as it stood when it was sent.

Inputs, pinned: the query set (dispatch://LEGION-386/artifact/query-set-json version 2, checked by
SHA-256) and the labels labels.py derives from the session store. Every issue in AGENTC, LEGION, OPS
and LEGSMOKE is rebuilt as a run of states: its title (the latest whole-issue event before a moment:
issue.created, issue.updated or issue.closed) and its primary spec (the latest version before it), each
state valid on (lo, hi]. A search sent at t sees every issue created before t in the state valid at t.

Systems, each over that as-of corpus of titles and primary specs:
  A        today's production search, emulated: websearch_to_tsquery (every word must match) over key and
           title (weight A) and, as a second row, the primary spec with ask blocks removed, ranked by
           ts_rank_cd then last update, 50 rows, collapsed to issues (api/search.go at the pinned commit)
  A:ran    what production actually returned, from the transcript (agent searches only)
  B        the keyword leg of the design: any word matching, ts_rank_cd / (1 + log length), key and title
           weight A, spec weight D
  C:<m>    meaning only: an issue's score is its best chunk's cosine similarity to the query
  W<w>:<m> C and B merged by reciprocal rank fusion (k = 60) over each one's top 200, B weighted w
  F:<m>+<r>, H<w>:<m>+<r>  C's or W<w>'s top 150 reranked (rerank command, on a sample)

Run order (STORE holds internal company text and is kept, mode 0700; SEARCHBENCH_PG is a scratch Postgres 16
with pgvector, started as bench.py's docstring says):
  measured.py labels  --store STORE        labels.py's rules over the session store -> STORE/labels.jsonl
  measured.py export  --store STORE        GET-only reads of production: histories, spec versions, states
  measured.py load    --store STORE        states into SEARCHBENCH_PG for the keyword systems
  measured.py embed   --store STORE --model M --budget-usd X     every distinct chunk text once, then every query
  measured.py score   --store STORE --models M...   first-stage systems for every scored query, lists to depth 200
  measured.py rerank  --store STORE --model M --reranker R --weights W... --per-class N --budget-usd X
  measured.py report  --store STORE [--compare A,B ...]   class tables, session-bootstrap intervals, results.json
Every command appends its run to STORE/runs.jsonl: arguments, times, load average, the command as executed,
`git rev-parse HEAD` and `git status --porcelain` of the checkout, and the SHA-256 of each harness file.
Provider usage goes to STORE/usage.jsonl, priced at bench.PRICES; the run stops at CAP_USD.
"""

import argparse
import bisect
import collections
import gzip
import hashlib
import json
import math
import os
import random
import re
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone

for _v in ("OPENBLAS_NUM_THREADS", "OMP_NUM_THREADS", "MKL_NUM_THREADS"):
    os.environ.setdefault(_v, "2")  # the devbox is shared: two threads for the matrix products

import numpy as np  # noqa: E402

import bench
import corpus
import dget
import providers as P
import replay

PROJECTS = ("AGENTC", "LEGION", "OPS", "LEGSMOKE")
QUERY_SET = ("LEGION-386", "query-set-json", 2, "3119782c2d0683bb32861911dd609786bde9d363552fc6c3cdf4eec697b91fb2")
FAR = datetime(9999, 1, 1, tzinfo=timezone.utc)
PROD_LIMIT = 50  # rows, before collapsing to issues (the harness has always asked for 50)
DEPTH = bench.DEPTH  # ranks beyond 150 count as a miss
FUSE_DEPTH = bench.FUSE_DEPTH
WEIGHTS = (0.1, 0.25, 0.5, 0.75, 1.0, 1.5, 2.0, 3.0)
# Labels scored as targets. `ambiguous` opens are counted but never scored.
TARGET_RULES = ("returned", "elsewhere", "chain", "chain-returned", "dashboard-opened")


def sha(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def utcnow() -> str:
    return datetime.now(timezone.utc).isoformat()


def ts(s: str) -> datetime:
    return replay.ts(s if s.endswith("Z") else s.replace("+00:00", "Z"))


def iso(d: datetime) -> str:
    return d.isoformat().replace("+00:00", "Z")


def jsonl(path: str) -> list[dict]:
    opener = gzip.open if path.endswith(".gz") else open
    with opener(path, "rt") as f:
        return [json.loads(line) for line in f]


def write_jsonl(path: str, rows) -> None:
    tmp = path + ".tmp"
    opener = gzip.open if path.endswith(".gz") else open
    with opener(tmp, "wt") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")
    os.replace(tmp, path)


# ---------------------------------------------------------------------------------------------
# queries: the scored set


def query_class(shape: dict | None) -> str:
    """The run's query classes, from the query set's own shape fields."""
    if not shape:
        return "empty"
    if shape["class"] == "literal":
        return "exact-token"
    if shape["class"] == "mixed":
        return "mixed"
    if "quoted" in shape["syntax"]:
        return "quoted phrase"
    return {"keywords": "short prose", "phrase": "long phrase", "question": "question"}[shape["prose_form"]]


def load_query_set(store: str) -> dict:
    path = os.path.join(store, "query-set-v2.json")
    with open(path, "rb") as f:
        body = f.read()
    if hashlib.sha256(body).hexdigest() != QUERY_SET[3]:
        raise SystemExit(f"{path} does not hash to the pinned query set {QUERY_SET[3]}")
    return json.loads(body)


def scored_queries(store: str) -> tuple[list[dict], dict]:
    """Every labelled search, and every duplicate pair, as one scored query each, with the counts of
    what was left out and why."""
    qs = load_query_set(store)
    by_id = {q["id"]: q for q in qs["queries"]}
    labels = {r["id"]: r for r in jsonl(os.path.join(store, "labels.jsonl"))}
    states = os.path.join(store, "corpus", "issues.json")
    created = {k: v["created_at"] for k, v in json.load(open(states)).items()} if os.path.exists(states) else None
    out, skipped = [], collections.Counter()
    for qid, lab in labels.items():
        q = by_id[qid]
        if q["source"] == "fleet-transcript" and not q["reached_server"]:
            skipped["did not reach the server"] += 1
            continue
        if q.get("context") == "test rig":
            skipped["test rig"] += 1
            continue
        if not isinstance(q.get("text"), str) or not q["text"].strip():
            skipped["no query text"] += 1
            continue
        targets = [x for x in lab.get("labels", []) if x["rule"] in TARGET_RULES]
        if not targets:
            skipped["no label"] += 1
            continue
        if created is not None:
            keep = []
            for x in targets:
                if x["key"] not in created:
                    skipped[f"target {x['rule']}: no such issue"] += 1
                elif ts(created[x["key"]]) >= ts(q["at"]):
                    skipped[f"target {x['rule']}: created after the search"] += 1
                elif q.get("project") and not x["key"].startswith(q["project"] + "-"):
                    skipped[f"target {x['rule']}: outside the searched project"] += 1
                else:
                    keep.append(x)
            if not keep:
                skipped["every target unfindable"] += 1
                continue
            targets = keep
        out.append({"id": qid, "source": q["source"], "text": q["text"], "at": q["at"], "project": q.get("project") or "",
                    "session": q.get("session") or q.get("browser_profile"), "class": query_class(q["shape"]),
                    "targets": targets, "hits": lab.get("hits"), "exclude": []})
    for p in qs["pairs"]:
        if p["state"] != "closed":
            skipped["pair parked, not closed"] += 1
            continue
        if not p["survivor_existed_at_filing"]:
            skipped["pair: survivor filed after the duplicate"] += 1
            continue
        for mode in ("title", "spec"):
            out.append({"id": f"pair:{p['duplicate']}:{mode}", "source": "pairs", "pair": p, "mode": mode,
                        "at": p["duplicate_created_at"], "project": "", "session": "pair:" + p["duplicate"],
                        "class": "duplicate title" if mode == "title" else "duplicate spec",
                        "targets": [{"key": p["survivor"], "rule": "pair"}], "hits": None, "exclude": [p["duplicate"]]})
    return out, dict(skipped)


# ---------------------------------------------------------------------------------------------
# export: GET-only reads of production, and the states


def pair_query_text(store: str, q: dict) -> str:
    """The pair's query as the replay built it: the duplicate's title as filed, or that title, a blank
    line and spec version 1 with ask blocks and every sentence naming the survivor removed."""
    data = os.path.join(store, "export")
    hist = replay.issue_history(data, q["pair"]["duplicate"])
    created = hist["snapshots"][0]
    if created["title"] != q["pair"]["query_title_as_filed"]:
        raise SystemExit(f"{q['id']}: issue.created title differs from the query set's")
    aid = created["primary_artifact_id"]
    v1 = replay.cget(data, f"/artifacts/{aid}/versions/1")["markdown"] if aid else None
    title_q, spec_q, _ = bench.query_texts(created["title"], v1, q["pair"]["survivor"])
    return title_q if q["mode"] == "title" else spec_q


def cmd_labels(args):
    import labels

    labels.derive(load_query_set(args.store), os.path.join(args.store, "labels.jsonl"))


def cmd_export(args):
    store, data = args.store, os.path.join(args.store, "export")
    os.umask(0o077)
    os.makedirs(os.path.join(store, "corpus"), mode=0o700, exist_ok=True)
    health_start, started = dget.health(), utcnow()
    keys = [i["key"] for proj in PROJECTS for i in replay.cget(data, "/issues", {"project": proj})]
    with ThreadPoolExecutor(args.threads) as ex:
        hists = {h["key"]: h for h in ex.map(lambda k: replay.issue_history(data, k), keys)}
    artifacts = sorted({s["primary_artifact_id"] for h in hists.values() for s in h["snapshots"] if s["primary_artifact_id"]})
    with ThreadPoolExecutor(args.threads) as ex:
        metas = dict(zip(artifacts, ex.map(lambda a: replay.cget(data, f"/artifacts/{a}"), artifacts)))
    print(f"{len(keys)} issues, {len(artifacts)} primary documents, "
          f"{sum(len(m['versions']) for m in metas.values())} versions", flush=True)

    # The moments a search looked at Dispatch.
    queries, _ = scored_queries(store)  # before issues.json exists, targets are not yet checked
    moments = sorted(ts(q["at"]) for q in queries)
    print(f"{len(moments)} search moments", flush=True)

    issues, intervals, needed = {}, [], {}
    for key, h in hists.items():
        created = ts(h["created_at"])
        points = {ts(s["at"]) for s in h["snapshots"]}
        for aid in {s["primary_artifact_id"] for s in h["snapshots"] if s["primary_artifact_id"]}:
            points |= {ts(v["created_at"]) for v in metas[aid]["versions"]}
        points = sorted(p for p in points if p > created)
        bounds = [created] + points + [FAR]
        issues[key] = {"project": key.split("-")[0], "created_at": h["created_at"]}
        for lo, hi in zip(bounds, bounds[1:]):
            a = bisect.bisect_right(moments, lo)
            b = bisect.bisect_right(moments, hi)
            if a == b:
                continue  # no search looked at the issue in this state
            needed[(key, lo, hi)] = True
    print(f"{len(needed)} issue states seen by at least one search", flush=True)

    def state(item):
        key, lo, hi = item
        at = replay.issue_at(data, hists[key], hi)
        snaps = [s for s in hists[key]["snapshots"] if ts(s["at"]) < hi]
        return key, lo, hi, at, max((s["at"] for s in snaps), default=hists[key]["created_at"])

    states, rows = {}, []
    with ThreadPoolExecutor(args.threads) as ex:
        for key, lo, hi, at, updated in ex.map(state, list(needed)):
            spec = at["spec"]
            sid = f"{key}:{sha(at['title'])[:10]}:" + (f"{spec['artifact_id'][:8]}v{spec['version']}" if spec else "none")
            if sid not in states:
                states[sid] = {"id": sid, "key": key, "title": at["title"], "title_event": at["title_event"],
                               "spec": {k: v for k, v in spec.items() if k != "markdown"} if spec else None,
                               "markdown": spec["markdown"] if spec else None}
            rows.append({"state": sid, "key": key, "lo": iso(lo), "hi": None if hi == FAR else iso(hi), "updated_at": updated})
    write_jsonl(os.path.join(store, "corpus", "states.jsonl"), states.values())
    write_jsonl(os.path.join(store, "corpus", "intervals.jsonl"), sorted(rows, key=lambda r: (r["key"], r["lo"])))
    with open(os.path.join(store, "corpus", "issues.json"), "w") as f:
        json.dump(issues, f)
    with open(os.path.join(store, "corpus", "export.json"), "w") as f:
        json.dump({"projects": PROJECTS, "snapshot_events": replay.SNAPSHOT_EVENTS, "started_at": started,
                   "finished_at": utcnow(), "production_health": {"start": health_start, "end": dget.health()},
                   "issues": len(keys), "primary_documents": len(artifacts), "states": len(states),
                   "intervals": len(rows)}, f)
    print(f"{len(states)} distinct states in {len(rows)} intervals", flush=True)



# ---------------------------------------------------------------------------------------------
# corpus in memory: states, intervals, chunks


class Corpus:
    """The as-of corpus: states, the intervals each is valid on, and each state's chunk texts."""

    def __init__(self, store: str):
        self.store = store
        cdir = os.path.join(store, "corpus")
        self.states = {s["id"]: s for s in jsonl(os.path.join(cdir, "states.jsonl"))}
        self.issues = json.load(open(os.path.join(cdir, "issues.json")))
        self.sids = sorted(self.states)
        self.sidx = {s: i for i, s in enumerate(self.sids)}
        ivs = jsonl(os.path.join(cdir, "intervals.jsonl"))
        self.iv_state = np.array([self.sidx[r["state"]] for r in ivs], np.int64)
        self.iv_key = np.array([r["key"] for r in ivs])
        self.iv_lo = np.array([ts(r["lo"]).timestamp() for r in ivs])
        self.iv_hi = np.array([ts(r["hi"]).timestamp() if r["hi"] else math.inf for r in ivs])
        self.iv_project = np.array([k.split("-")[0] for k in self.iv_key])
        # chunks: each state's chunk texts, as the harness chunks a spec (title-prefixed, ask blocks removed)
        self.texts: dict[str, str] = {}
        self.state_chunks: list[list[str]] = []
        for sid in self.sids:
            s = self.states[sid]
            hs = []
            for text in corpus.chunk_markdown(s["title"], corpus.strip_ask_blocks(s["markdown"] or "")):
                h = sha(text)
                self.texts[h] = text
                hs.append(h)
            self.state_chunks.append(hs)
        self.hashes = sorted(self.texts)
        self.hidx = {h: i for i, h in enumerate(self.hashes)}
        self.rows = np.array([self.hidx[h] for hs in self.state_chunks for h in hs], np.int64)
        self.ptr = np.cumsum([0] + [len(hs) for hs in self.state_chunks])[:-1]

    def live(self, q: dict) -> np.ndarray:
        """Indices of the intervals a query sees: one per issue created before it, the duplicate excluded."""
        t = ts(q["at"]).timestamp()
        m = (self.iv_lo < t) & (t <= self.iv_hi)
        if q["project"]:
            m &= self.iv_project == q["project"]
        for k in q["exclude"]:
            m &= self.iv_key != k
        return np.nonzero(m)[0]


# ---------------------------------------------------------------------------------------------
# load: states into Postgres for the keyword systems


def cmd_load(args):
    c = Corpus(args.store)
    conn = bench.connect()
    conn.execute(
        """drop table if exists m_interval; drop table if exists m_state;
           create table m_state (id text primary key, key text not null, project text not null, title text not null,
             prod_title tsvector not null, prod_spec tsvector, kw tsvector not null);
           create table m_interval (state_id text not null references m_state, key text not null, lo timestamptz not null,
             hi timestamptz, updated_at timestamptz not null);"""
    )
    with conn.cursor() as cur:
        cur.executemany(
            """insert into m_state values (%(id)s, %(key)s, %(project)s, %(title)s,
                 setweight(to_tsvector('english', %(key)s), 'A') || setweight(to_tsvector('english', %(title)s), 'A'),
                 case when %(spec)s::text is null then null
                      else to_tsvector('english', regexp_replace(%(spec)s::text, '(?s):::ask\\{.*?:::', '', 'g')) end,
                 setweight(to_tsvector('english', %(key)s), 'A') || setweight(to_tsvector('english', %(title)s), 'A')
                   || setweight(to_tsvector('english', %(body)s), 'D'))""",
            [{"id": s["id"], "key": s["key"], "project": s["key"].split("-")[0], "title": s["title"],
              "spec": s["markdown"] if s["spec"] else None, "body": corpus.strip_ask_blocks(s["markdown"] or "")}
             for s in c.states.values()],
        )
        ivs = jsonl(os.path.join(args.store, "corpus", "intervals.jsonl"))
        cur.executemany("insert into m_interval values (%s,%s,%s,%s,%s)",
                        [(r["state"], r["key"], r["lo"], r["hi"], r["updated_at"]) for r in ivs])
    conn.execute("create index on m_interval (lo); analyze;")
    print(f"{len(c.states)} states, {len(c.texts)} distinct chunk texts ({sum(map(len, c.texts.values())):,} characters)")


LIVE_SQL = """select iv.key, iv.state_id, iv.updated_at from m_interval iv join m_state s on s.id = iv.state_id
               where iv.lo < %(t)s and (iv.hi is null or %(t)s <= iv.hi)
                 and (%(proj)s = '' or s.project = %(proj)s) and iv.key <> all(%(exclude)s)"""


def sys_production(conn, q: dict, text: str) -> tuple[list[tuple[str, float]], str | None]:
    """Today's search over the as-of titles and primary specs: every word must match, 50 rows, then
    one entry per issue at its first row."""
    if len(text.strip()) < 2:
        return [], "refused: fewer than 2 characters"
    if conn.execute("select numnode(websearch_to_tsquery('english', %s))", (text.strip(),)).fetchone()[0] == 0:
        return [], None
    rows = conn.execute(
        f"""with q as (select websearch_to_tsquery('english', %(q)s) as tsq), live as ({LIVE_SQL}),
            hits as (select 'issue' as kind, l.key, ts_rank_cd(s.prod_title, q.tsq) as rank, l.updated_at
                       from live l join m_state s on s.id = l.state_id, q where s.prod_title @@ q.tsq
                     union all
                     select 'document', l.key, ts_rank_cd(s.prod_spec, q.tsq), l.updated_at
                       from live l join m_state s on s.id = l.state_id, q where s.prod_spec @@ q.tsq)
            select key, rank from hits order by rank desc, updated_at desc, kind, key limit %(n)s""",
        {"q": text.strip(), "t": q["at"], "proj": q["project"], "exclude": q["exclude"], "n": PROD_LIMIT},
    ).fetchall()
    out, seen = [], set()
    for key, rank in rows:
        if key not in seen:
            seen.add(key)
            out.append((key, float(rank)))
    return out, None


def sys_keyword(conn, q: dict, text: str) -> list[tuple[str, float]]:
    rows = conn.execute(
        f"""with q as (select {bench.KW_TSQUERY} as tsq), live as ({LIVE_SQL})
            select l.key, ts_rank_cd(s.kw, q.tsq, 1) as r from live l join m_state s on s.id = l.state_id, q
             where s.kw @@ q.tsq order by r desc, l.key limit %(n)s""",
        {"q": bench.keyword_text(text), "t": q["at"], "proj": q["project"], "exclude": q["exclude"], "n": FUSE_DEPTH},
    ).fetchall()
    return [(k, float(r)) for k, r in rows]


# ---------------------------------------------------------------------------------------------
# vectors: kept under STORE/vectors/<model>/, addressed by the SHA-256 of the text embedded


class VectorStore:
    """Append-only shards of (sha256, vector) per model and role: <role>-NNNN.npy with <role>-NNNN.sha256."""

    def __init__(self, store: str, model: str, role: str):
        self.dir = os.path.join(store, "vectors", model)
        self.role = role
        os.makedirs(self.dir, mode=0o700, exist_ok=True)

    def names(self) -> list[str]:
        return sorted(n[:-4] for n in os.listdir(self.dir) if n.startswith(self.role + "-") and n.endswith(".npy"))

    def load(self) -> dict[str, np.ndarray]:
        out = {}
        for n in self.names():
            with open(os.path.join(self.dir, n + ".sha256")) as f:
                hs = [line.strip() for line in f if line.strip()]
            m = np.load(os.path.join(self.dir, n + ".npy"))
            if len(hs) != len(m):
                raise SystemExit(f"{self.dir}/{n}: {len(hs)} hashes for {len(m)} vectors")
            out.update(zip(hs, m))
        return out

    def append(self, hashes: list[str], vecs: np.ndarray) -> None:
        base = os.path.join(self.dir, f"{self.role}-{len(self.names()):04d}")
        with open(base + ".sha256", "w") as f:
            f.write("\n".join(hashes) + "\n")
        np.save(base + ".tmp.npy", vecs.astype(np.float32))
        os.replace(base + ".tmp.npy", base + ".npy")  # the .npy lands last: a shard is whole or absent


def process_spend() -> float:
    return replay.process_spend()


def log_usage(store: str, label: str) -> None:
    usage = {k: dict(v) for k, v in P.USAGE.items()}
    with open(os.path.join(store, "usage.jsonl"), "a") as f:
        f.write(json.dumps({"at": utcnow(), "label": label, "usage": usage, "usd": round(process_spend(), 4)}) + "\n")


def query_texts(store: str, queries: list[dict]) -> dict[str, str]:
    """Each scored query's text: the search as sent, or the pair's query built from its duplicate."""
    return {q["id"]: pair_query_text(store, q) if q["source"] == "pairs" else q["text"] for q in queries}


def cmd_embed(args):
    c = Corpus(args.store)
    queries, _ = scored_queries(args.store)
    texts = query_texts(args.store, queries)
    emb = P.EMBEDDERS[args.model]()
    emb.concurrency = args.concurrency
    before = ledger_spend(args.store)
    prov = next(p for p in bench.PRICES if p.endswith(":" + getattr(emb, "model_id", emb.name)))
    price = bench.PRICES[prov][1]
    pools = (("query", {sha(t): t for t in texts.values()}), ("document", c.texts))
    todo = {}
    for role, pool in pools:
        have = set(VectorStore(args.store, args.model, role).load())
        todo[role] = sorted((h for h in pool if h not in have), key=lambda h: len(pool[h]))
    chars = sum(len(pool[h][: emb.max_chars]) for role, pool in pools for h in todo[role])
    estimate = chars / 4 * price  # about four characters a token
    print(f"{args.model}: {sum(map(len, todo.values()))} texts, {chars:,} characters, about ${estimate:.2f} "
          f"at {prov}'s list price; run spend so far ${before:.2f} of ${CAP_USD:.2f}", flush=True)
    if args.dry_run:
        return
    if before + estimate > CAP_USD:
        raise SystemExit(f"refused: the estimate would take the run past its ${CAP_USD:.2f} cap")
    try:
        for role, pool in pools:
            vs = VectorStore(args.store, args.model, role)
            print(f"{args.model} {role}: {len(todo[role])} of {len(pool)} texts to embed", flush=True)
            t0 = time.time()
            for s in range(0, len(todo[role]), args.step):
                part = todo[role][s : s + args.step]
                vs.append(part, emb.embed([pool[h] for h in part], role))
                done = s + len(part)
                print(f"  {done}/{len(todo[role])}  {done / (time.time() - t0):.1f}/s  ${process_spend():.3f}", flush=True)
                check_spend(args.store, before, args.budget_usd)
    finally:
        log_usage(args.store, f"embed {args.model}")


CAP_USD = 15.0  # this run's cap across every process, from its own ledger


def ledger_spend(store: str) -> float:
    path = os.path.join(store, "usage.jsonl")
    return sum(r["usd"] for r in jsonl(path)) if os.path.exists(path) else 0.0


def check_spend(store: str, before: float, budget: float) -> None:
    """Stop once this process passes its own budget or the run passes its cap."""
    spent = process_spend()
    if spent > budget:
        raise SystemExit(f"stopped: this process has spent ${spent:.2f}, over its ${budget:.2f} budget")
    if before + spent > CAP_USD:
        raise SystemExit(f"stopped: the run has spent ${before + spent:.2f}, over its ${CAP_USD:.2f} cap")


# ---------------------------------------------------------------------------------------------
# score: the first-stage systems for every scored query


def rank_of(keys: list[str], targets: list[dict]) -> int | None:
    """The best rank of any target, counting from 1; None beyond DEPTH or absent."""
    want = {t["key"] for t in targets}
    for r, k in enumerate(keys[:DEPTH], 1):
        if k in want:
            return r
    return None


class Dense:
    """Meaning-only search over the as-of corpus with one model's stored vectors."""

    def __init__(self, c: Corpus, store: str, model: str):
        self.c, self.model = c, model
        vecs = VectorStore(store, model, "document").load()
        missing = [h for h in c.hashes if h not in vecs]
        if missing:
            raise SystemExit(f"{model}: {len(missing)} chunk texts have no vector; run embed first")
        self.m = np.stack([vecs[h] for h in c.hashes]).astype(np.float32)
        self.qv = VectorStore(store, model, "query").load()

    def sims(self, text: str) -> np.ndarray:
        v = self.qv.get(sha(text))
        if v is None:
            raise SystemExit(f"{self.model}: no query vector for {sha(text)[:12]}; run embed first")
        return self.m @ v

    def rank(self, q: dict, sims: np.ndarray, live: np.ndarray, n: int = FUSE_DEPTH) -> list[tuple[str, float]]:
        best = np.maximum.reduceat(sims[self.c.rows], self.c.ptr)
        st = self.c.iv_state[live]
        keys = self.c.iv_key[live]
        scores = best[st]
        order = np.lexsort((keys, -scores))[:n]
        return [(str(keys[i]), float(scores[i])) for i in order]


def fused(dense: list[tuple[str, float]], kw: list[tuple[str, float]], w: float, n: int = FUSE_DEPTH) -> list[tuple[str, float]]:
    """bench.rrf_weighted's order (k = 60, keyword contribution times w), with each item's fused score."""
    score: dict[str, float] = {}
    for rank, (k, _) in enumerate(dense, 1):
        score[k] = score.get(k, 0.0) + 1.0 / (bench.RRF_K + rank)
    for rank, (k, _) in enumerate(kw, 1):
        score[k] = score.get(k, 0.0) + w / (bench.RRF_K + rank)
    return [(k, round(score[k], 8)) for k in sorted(score, key=lambda k: (-score[k], k))[:n]]


def cmd_score(args):
    c = Corpus(args.store)
    queries, skipped = scored_queries(args.store)
    texts = query_texts(args.store, queries)
    conn = bench.connect()
    models = {m: Dense(c, args.store, m) for m in args.models}
    sdir = os.path.join(args.store, "scores")
    os.makedirs(sdir, mode=0o700, exist_ok=True)
    t0 = time.time()
    ranks_path, lists_path = os.path.join(sdir, "first-stage.jsonl.gz"), os.path.join(sdir, "first-stage-lists.jsonl.gz")
    with gzip.open(ranks_path + ".tmp", "wt") as fr, gzip.open(lists_path + ".tmp", "wt") as fl:
        for n, q in enumerate(queries, 1):
            text = texts[q["id"]]
            live = c.live(q)
            live_keys = set(c.iv_key[live].tolist())
            lists = {}
            prod, err = sys_production(conn, q, text)
            lists["A"], lists["B"] = prod, sys_keyword(conn, q, text)
            if q["hits"] is not None:
                lists["A:ran"] = [(k, None) for k in q["hits"]]
            for m, d in models.items():
                dense = d.rank(q, d.sims(text), live)
                lists[f"C:{m}"] = [(k, round(s, 6)) for k, s in dense]
                for w in WEIGHTS:
                    lists[f"W{w}:{m}"] = fused(dense, lists["B"], w)
            rec = {"id": q["id"], "class": q["class"], "source": q["source"], "at": q["at"], "session": q["session"],
                   "text_sha256": sha(text), "targets": q["targets"], "candidates": len(live),
                   "findable": any(t["key"] in live_keys for t in q["targets"]), "production_error": err,
                   "ranks": {s: rank_of([k for k, _ in lst], q["targets"]) for s, lst in lists.items()},
                   "top10": {s: [k for k, _ in lst[:10]] for s, lst in lists.items()}}
            fr.write(json.dumps(rec) + "\n")
            fl.write(json.dumps({"id": q["id"], "text_sha256": rec["text_sha256"], "lists": lists}) + "\n")
            if n % 200 == 0:
                print(f"{n}/{len(queries)} scored, {time.time() - t0:.0f}s", flush=True)
    os.replace(ranks_path + ".tmp", ranks_path)
    os.replace(lists_path + ".tmp", lists_path)
    with open(os.path.join(sdir, "first-stage.meta.json"), "w") as f:
        json.dump({"scored": len(queries), "skipped": skipped, "models": args.models, "weights": WEIGHTS,
                   "depth": FUSE_DEPTH, "production_rows": PROD_LIMIT, "finished_at": utcnow()}, f)
    print(f"{len(queries)} queries scored; skipped {skipped}")


# ---------------------------------------------------------------------------------------------
# rerank: a stratified sample, each candidate list's top 150 reranked


def stratified_sample(rows: list[dict], per_class: int, seed: int) -> list[dict]:
    """Up to per_class queries from each class, drawn with a fixed seed; every duplicate-pair query."""
    rng = random.Random(seed)
    by = collections.defaultdict(list)
    for r in rows:
        by[r["class"]].append(r)
    out = []
    for cls in sorted(by):
        pool = sorted(by[cls], key=lambda r: r["id"])
        out += pool if cls.startswith("duplicate") else rng.sample(pool, min(per_class, len(pool)))
    return out


class RerankCache:
    """Every (query, document) score a reranker returned, kept under STORE/rerank/<reranker>.jsonl."""

    def __init__(self, store: str, reranker: str):
        os.makedirs(os.path.join(store, "rerank"), mode=0o700, exist_ok=True)
        self.path = os.path.join(store, "rerank", reranker + ".jsonl")
        self.scores = {r["k"]: r["score"] for r in jsonl(self.path)} if os.path.exists(self.path) else {}

    def put(self, pairs: list[tuple[str, str, str, float]]) -> None:
        with open(self.path, "a") as f:
            for k, qh, dh, s in pairs:
                self.scores[k] = s
                f.write(json.dumps({"k": k, "query_sha256": qh, "doc_sha256": dh, "score": s}) + "\n")


def passages_for(c: Corpus, d: Dense, sims: np.ndarray, live: np.ndarray, keys: list[str]) -> dict[str, str]:
    """Each issue as the reranker sees it: its best three chunks by this model's similarity (bench.passage_docs)."""
    state_of = {str(c.iv_key[i]): int(c.iv_state[i]) for i in live}
    rows = []
    for k in keys:
        s = state_of[k]
        hs = c.state_chunks[s]
        idx = [c.hidx[h] for h in hs]
        order = sorted(range(len(hs)), key=lambda j: (-float(sims[idx[j]]), j))
        rows += [(k, c.texts[hs[j]], float(sims[idx[j]])) for j in order]
    return bench.passage_docs(rows)


def cmd_rerank(args):
    c = Corpus(args.store)
    queries, _ = scored_queries(args.store)
    texts = query_texts(args.store, queries)
    qmap = {q["id"]: q for q in queries}
    first = {r["id"]: r for r in jsonl(os.path.join(args.store, "scores", "first-stage.jsonl.gz"))}
    sample = stratified_sample(list(first.values()), args.per_class, args.seed)
    wanted = {r["id"] for r in sample}
    kw_lists = {}
    with gzip.open(os.path.join(args.store, "scores", "first-stage-lists.jsonl.gz"), "rt") as f:
        for line in f:
            x = json.loads(line)
            if x["id"] in wanted:
                kw_lists[x["id"]] = [tuple(y) for y in x["lists"]["B"]]
    d = Dense(c, args.store, args.model)
    rr = P.RERANKERS[args.reranker]()
    cache = RerankCache(args.store, args.reranker)
    before = ledger_spend(args.store)
    out_path = os.path.join(args.store, "scores", f"rerank-{args.model}-{args.reranker}.jsonl.gz")
    done = {r["id"]: r for r in jsonl(out_path)} if os.path.exists(out_path) else {}
    depth = bench.RERANK_DEPTH
    print(f"{len(sample)} sampled queries ({len(done)} already reranked); run spend so far ${before:.2f}", flush=True)
    try:
        for n, r in enumerate(sample, 1):
            if r["id"] in done:
                continue
            q, text = qmap[r["id"]], texts[r["id"]]
            sims, live = d.sims(text), c.live(q)
            dense = d.rank(q, sims, live)
            kw = kw_lists[r["id"]]
            bases = {f"F:{args.model}+{args.reranker}": [k for k, _ in dense]}
            for w in args.weights:
                bases[f"H{w}:{args.model}+{args.reranker}"] = [k for k, _ in fused(dense, kw, w)]
            union = list(dict.fromkeys(k for b in bases.values() for k in b[:depth]))
            docs = passages_for(c, d, sims, live, union)
            qh = sha(text)
            keyed = {k: (sha(f"{text}\0{docs[k]}"), sha(docs[k])) for k in union}
            missing = [k for k in union if keyed[k][0] not in cache.scores]
            if missing:
                fresh = rr.rerank(text, [docs[k] for k in missing])
                cache.put([(keyed[k][0], qh, keyed[k][1], float(s)) for k, s in zip(missing, fresh)])
            score = {k: cache.scores[keyed[k][0]] for k in union}
            rec = {"id": r["id"], "class": r["class"], "targets": r["targets"], "scores": score, "ranks": {}, "top10": {},
                   "lists": {}}
            for name, base in bases.items():
                pos = {k: i for i, k in enumerate(base)}
                top = sorted(base[:depth], key=lambda k: (-score[k], pos[k]))
                keys = top + base[depth:]
                rec["ranks"][name] = rank_of(keys, r["targets"])
                rec["top10"][name] = keys[:10]
                # depth 200: the reranked top 150 with their scores, then the first stage's order
                rec["lists"][name] = [(k, score[k] if i < depth else None) for i, k in enumerate(keys[:FUSE_DEPTH])]
            done[r["id"]] = rec
            if n % 25 == 0 or n == len(sample):
                write_jsonl(out_path, done.values())
                print(f"  {n}/{len(sample)}  ${process_spend():.3f}", flush=True)
            check_spend(args.store, before, args.budget_usd)
    finally:
        write_jsonl(out_path, done.values())
        log_usage(args.store, f"rerank {args.model}+{args.reranker}")


# ---------------------------------------------------------------------------------------------
# report


CLASSES = ("exact-token", "mixed", "quoted phrase", "short prose", "long phrase", "question", "duplicate title", "duplicate spec")


def metrics(ranks: list[int | None]) -> dict:
    n = len(ranks)
    if not n:
        return {"n": 0}
    top = {k: sum(1 for r in ranks if r and r <= k) for k in (1, 5, 10)}
    return {"n": n, "mrr": round(sum(1 / r for r in ranks if r) / n, 4), "top1": top[1], "top5": top[5], "top10": top[10]}


def rr(rank: int | None) -> float:
    return 1 / rank if rank else 0.0


def cluster_boot(rows: list[dict], a: str, b: str, reps: int = 2000, seed: int = 0) -> dict:
    """MRR of a minus MRR of b, with a 95% interval from resampling sessions (searches in one session
    are not independent)."""
    by = collections.defaultdict(list)
    for r in rows:
        by[r["session"]].append(rr(r["ranks"][a]) - rr(r["ranks"][b]))
    groups = [np.array(v) for v in by.values()]
    sums = np.array([g.sum() for g in groups])
    lens = np.array([len(g) for g in groups])
    rng = np.random.default_rng(seed)
    idx = rng.integers(0, len(groups), size=(reps, len(groups)))
    boots = sums[idx].sum(1) / lens[idx].sum(1)
    lo, hi = np.percentile(boots, [2.5, 97.5])
    d = [x for g in groups for x in g]
    return {"diff": round(float(np.mean(d)), 4), "lo": round(float(lo), 4), "hi": round(float(hi), 4),
            "a_ahead": int(sum(x > 0 for x in d)), "b_ahead": int(sum(x < 0 for x in d)), "n": len(d), "sessions": len(groups)}


def merged_rows(store: str) -> tuple[list[dict], list[str]]:
    """First-stage rows with every rerank file's ranks merged in; the rerank systems are on the sample only."""
    rows = {r["id"]: r for r in jsonl(os.path.join(store, "scores", "first-stage.jsonl.gz"))}
    rerank_systems = []
    sdir = os.path.join(store, "scores")
    for name in sorted(os.listdir(sdir)):
        if name.startswith("rerank-") and name.endswith(".jsonl.gz"):
            for r in jsonl(os.path.join(sdir, name)):
                rows[r["id"]]["ranks"].update(r["ranks"])
                rows[r["id"]]["top10"].update(r["top10"])
                rerank_systems += [s for s in r["ranks"] if s not in rerank_systems]
    return list(rows.values()), rerank_systems


def cmd_report(args):
    rows, rerank_systems = merged_rows(args.store)
    systems = sorted({s for r in rows for s in r["ranks"]}, key=lambda s: (s[0], s))
    groups = {cls: [r for r in rows if r["class"] == cls] for cls in CLASSES}
    groups["all agent searches"] = [r for r in rows if r["source"] == "fleet-transcript"]
    groups["Sami's searches"] = [r for r in rows if r["source"] == "dashboard-sjawhar"]
    for rule in ("returned", "missed"):
        want = {"returned": {"returned"}, "missed": {"elsewhere", "chain"}}[rule]
        groups[f"agent searches, target {rule} by production"] = [
            r for r in groups["all agent searches"] if {t["rule"] for t in r["targets"]} <= want]
    table = {g: {s: metrics([r["ranks"][s] for r in rs if s in r["ranks"]]) for s in systems} for g, rs in groups.items()}
    comparisons = {}
    for pair in args.compare or []:
        a, b = pair.split(",")
        comparisons[pair] = {g: cluster_boot([r for r in rs if a in r["ranks"] and b in r["ranks"]], a, b)
                             for g, rs in groups.items() if any(a in r["ranks"] and b in r["ranks"] for r in rs)}
    queries, skipped = scored_queries(args.store)
    out = {"what": "Per-class results of Dispatch's search candidates on the measured query set (LEGION-386). "
                   "ranks count from 1 (the best-ranked labelled target), null beyond 150 or absent.",
           "systems": systems, "rerank_systems": rerank_systems, "groups": {g: len(rs) for g, rs in groups.items()},
           "table": table, "comparisons": comparisons, "skipped": skipped,
           "usage": jsonl(os.path.join(args.store, "usage.jsonl")) if os.path.exists(os.path.join(args.store, "usage.jsonl")) else [],
           "spend_usd": round(ledger_spend(args.store), 4)}
    os.makedirs(os.path.join(args.store, "report"), mode=0o700, exist_ok=True)
    with open(os.path.join(args.store, "report", "results.json"), "w") as f:
        json.dump(out, f, indent=1)
    show = args.systems or systems
    for g, rs in groups.items():
        print(f"\n### {g} ({len(rs)})\n")
        print("| system | n | MRR | top 1 | top 5 | top 10 |\n| --- | --- | --- | --- | --- | --- |")
        for s in show:
            m = table[g].get(s, {"n": 0})
            if m["n"]:
                print(f"| {s} | {m['n']} | {m['mrr']:.3f} | {m['top1']} | {m['top5']} | {m['top10']} |")
    for pair, by in comparisons.items():
        print(f"\n### {pair}\n")
        for g, c in by.items():
            print(f"- {g}: {c['diff']:+.3f} [{c['lo']:+.3f}, {c['hi']:+.3f}], ahead {c['a_ahead']} / {c['b_ahead']}, n {c['n']}")


# ---------------------------------------------------------------------------------------------


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    cmds = {"labels": cmd_labels, "export": cmd_export, "load": cmd_load, "embed": cmd_embed, "score": cmd_score,
            "rerank": cmd_rerank, "report": cmd_report}
    for name in cmds:
        p = sub.add_parser(name)
        p.add_argument("--store", required=True)
        if name == "export":
            p.add_argument("--threads", type=int, default=4)
        if name == "embed":
            p.add_argument("--model", required=True, choices=list(P.EMBEDDERS))
            p.add_argument("--budget-usd", type=float, required=True)
            p.add_argument("--step", type=int, default=192)
            p.add_argument("--concurrency", type=int, default=2)
            p.add_argument("--dry-run", action="store_true")
        if name == "score":
            p.add_argument("--models", nargs="+", required=True)
        if name == "rerank":
            p.add_argument("--model", required=True)
            p.add_argument("--reranker", required=True, choices=list(P.RERANKERS))
            p.add_argument("--weights", nargs="+", type=float, required=True, help="fused lists to rerank, by keyword weight")
            p.add_argument("--per-class", type=int, required=True)
            p.add_argument("--seed", type=int, default=386)
            p.add_argument("--budget-usd", type=float, required=True)
        if name == "report":
            p.add_argument("--systems", nargs="*", help="rows to print (all by default)")
            p.add_argument("--compare", nargs="*", help="'a,b' pairs for bootstrap intervals by class")
    args = ap.parse_args()
    os.umask(0o077)
    run = {"cmd": args.cmd, "args": {k: v for k, v in vars(args).items() if k != "cmd"}, "started_at": utcnow(),
           "loadavg_start": os.getloadavg(), "build": build_identity(),
           "harness": {n: _self_sha(n) for n in HARNESS_FILES}}
    try:
        cmds[args.cmd](args)
        run["outcome"] = "ok"
    except SystemExit as e:
        run["outcome"] = f"exit: {e.code}"
        raise
    except BaseException as e:
        run["outcome"] = f"{type(e).__name__}: {str(e)[:300]}"
        raise
    finally:
        run.update(finished_at=utcnow(), loadavg_end=os.getloadavg())
        with open(os.path.join(args.store, "runs.jsonl"), "a") as f:
            f.write(json.dumps(run) + "\n")


HARNESS_FILES = ("measured.py", "labels.py", "replay.py", "bench.py", "corpus.py", "dget.py", "providers.py")


def build_identity() -> dict:
    """What this process is: the command as executed, and the checkout it runs from (HEAD and whether
    the tree is clean), read from git; a failure to read either is recorded as that failure."""
    import shlex
    import subprocess

    here = os.path.dirname(os.path.abspath(__file__))
    out = {"command": shlex.join([sys.executable] + sys.argv), "cwd": os.getcwd(),
           "env": {k: os.environ[k] for k in ("SEARCHBENCH_PG", "AWS_PROFILE", "VIRTUAL_ENV") if k in os.environ}}
    for name, cmd in (("git_head", ["git", "rev-parse", "HEAD"]), ("git_status_porcelain", ["git", "status", "--porcelain"])):
        try:
            p = subprocess.run(cmd, cwd=here, capture_output=True, text=True, timeout=120)
            out[name] = p.stdout.strip() if p.returncode == 0 else f"failed ({p.returncode}): {p.stderr.strip()[:300]}"
        except Exception as e:  # recorded, never substituted
            out[name] = f"failed: {type(e).__name__}: {e}"
    return out


def _self_sha(name: str) -> str:
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), name), "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


if __name__ == "__main__":
    main()

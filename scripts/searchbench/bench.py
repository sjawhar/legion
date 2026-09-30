#!/usr/bin/env python3
"""Dispatch search benchmark on a local Postgres 16 + pgvector.

Pipeline (the data is internal company text: keep DATA outside any repository, mode 0700/0600):
  docker run -d --name searchbench-pg -e POSTGRES_PASSWORD=searchbench -e POSTGRES_DB=searchbench \
      -p 127.0.0.1:55439:5432 pgvector/pgvector:pg16
  export.py --out DATA --projects AGENTC LEGION OPS      issues, primary specs, asks (GET only)
  asof.py --data DATA                                    duplicate benchmark + filing-time texts
  sample_decisions.py DATA SEED N > DATA/../decision-sample.json, then write
      DATA/../decision-questions.json by hand: {"questions_by_ask": {"<ask id prefix>": "<question>"}}
  bench.py load   --data DATA                     items, chunks and the keyword index
  bench.py embed  --data DATA --model NAME        embed every chunk not yet embedded (resumable)
  bench.py run    --data DATA --out FILE [...]    run the query sets through the systems
  bench.py report --out FILE [FILE...] --data DATA  results tables, paired comparison, spend
Provider keys come from the environment (`secrets KEY -- uv run python bench.py ...`); Bedrock uses
the standard AWS credential chain (AWS_PROFILE), in us-west-2 with the US cross-region Cohere profile.

Systems, per query:
  A  production GET /search (today's search), hits collapsed to their issue or ask
  B  fixed keyword search: websearch terms joined with OR, ts_rank_cd normalised by log length,
     issues indexed as title (weight A) plus spec (weight D), asks as question (A) plus options
     and answer (D), one row per item; B:len is the same normalised by length instead
  C  dense only: max cosine over an item's chunks, one row per item
  D  B and C fused with reciprocal rank fusion (k=60) over each one's top 200
  E  D's top 150 reranked; each item is shown to the reranker as its best three passages by the
     embedder's similarity to the query
  F  C's top 150 reranked the same way (--rerank-input dense)
  W<w>  C and B fused by RRF with B's contribution weighted w (0.25, 0.5, 0.75)
  T  C's order, with B's rank breaking ties among items within TIE_BAND cosine of each other
  P<b>  C with b added to results sharing the duplicate's parent or a component (as filed);
        decision searches are left in C's order
  Q  E for decision searches, C for duplicate searches (rerank questions only)
A name ending @N reranked only the top N (--rerank-depth N).

The database URL comes from SEARCHBENCH_PG (default postgresql://postgres:searchbench@127.0.0.1:55439/searchbench).

Here only the survivor is scored as it stood at filing; every other candidate has its current text.
replay.py runs the duplicate half as a historical replay: every candidate as it stood, and today's
search on a scratch Dispatch holding that day's issues.
"""

import argparse
import hashlib
import json
import math
import os
import re
import statistics
import time

import numpy as np
import psycopg
from pgvector.psycopg import register_vector

import corpus
import dget
import providers as P

PG = os.environ.get("SEARCHBENCH_PG", "postgresql://postgres:searchbench@127.0.0.1:55439/searchbench")
RRF_K = 60
FUSE_DEPTH = 200
RERANK_DEPTH = 150
PASSAGES = 3
DEPTH = 150  # ranks beyond this count as a miss


def connect():
    conn = psycopg.connect(PG, autocommit=True)
    conn.execute("create extension if not exists vector")
    register_vector(conn)
    conn.execute(
        """create table if not exists qcache (model text, text_hash text, v vector, latency float8, primary key (model, text_hash));
           create table if not exists rcache (reranker text, key text, scores float8[], latency float8, primary key (reranker, key));
           create table if not exists rdoc (reranker text, key text, score float8, primary key (reranker, key));"""
    )
    return conn


def slug(model: str) -> str:
    return "emb_" + re.sub(r"[^a-z0-9]+", "_", model.lower())


# ---------------------------------------------------------------------------------------------
# load / embed


def cmd_load(args):
    items, chunks = corpus.load(args.data)
    conn = connect()
    conn.execute(
        """
        drop table if exists items cascade;
        drop table if exists chunks cascade;
        create table items (id text primary key, kind text not null, issue_key text, created_at timestamptz not null,
                            answered_at timestamptz, asof_for text, kw tsvector not null);
        create table chunks (id text primary key, item_id text not null references items, ord int not null, text text not null);
        """
    )
    with conn.cursor() as cur:
        cur.executemany(
            "insert into items values (%s,%s,%s,%s,%s,%s, setweight(to_tsvector('english', %s), 'A') || setweight(to_tsvector('english', %s), 'D'))",
            [(i.id, i.kind, i.issue_key, i.created_at, i.answered_at, i.asof_for, i.keyword_title, i.keyword_body) for i in items.values()],
        )
        cur.executemany(
            "insert into chunks values (%s,%s,%s,%s)",
            [(c.id, c.item_id, int(c.id.rsplit("#", 1)[1]), c.text) for c in chunks],
        )
    conn.execute("create index on items using gin (kw); create index on chunks (item_id); analyze;")
    print(f"loaded {len(items)} items, {len(chunks)} chunks")


def cmd_embed(args):
    emb = P.EMBEDDERS[args.model]()
    conn = connect()
    table = slug(args.model)
    conn.execute(f"create table if not exists {table} (chunk_id text primary key references chunks, v vector({emb.dim}))")
    todo = conn.execute(f"select c.id, c.text from chunks c left join {table} e on e.chunk_id = c.id where e.chunk_id is null order by length(c.text)").fetchall()
    shard, shards = (int(x) for x in args.shard.split("/"))
    todo = [row for row in todo if int(hashlib.sha256(row[0].encode()).hexdigest(), 16) % shards == shard]
    print(f"{args.model}: {len(todo)} chunks to embed (shard {args.shard})")
    step = args.step
    t0 = time.time()
    for s in range(0, len(todo), step):
        part = todo[s : s + step]
        vecs = emb.embed([t for _, t in part], "document")
        with conn.cursor() as cur:
            cur.executemany(f"insert into {table} values (%s, %s)", [(cid, v) for (cid, _), v in zip(part, vecs)])
        done = s + len(part)
        rate = done / (time.time() - t0)
        print(f"  {done}/{len(todo)}  {rate:.1f} chunks/s  eta {(len(todo) - done) / rate / 60:.1f} min  usage={dict(P.USAGE)}", flush=True)
    log_usage(args.data, f"embed {args.model}")


# ---------------------------------------------------------------------------------------------
# queries


def query_texts(dup_title: str, dup_spec: str | None, survivor: str) -> tuple[str, str, int]:
    """The two searches a filer would have run: the duplicate's title as filed, and that title, a
    blank line and spec version 1 with ask blocks and every sentence naming the survivor removed.
    Returns (title query, spec query, characters removed as naming the survivor)."""
    spec = corpus.strip_ask_blocks(dup_spec or "")
    # Drop sentences that name the surviving issue: a filer who already knew it would not be searching.
    leak = re.compile(rf"[^\n.]*\b{re.escape(survivor)}\b[^\n.]*[.]?")
    spec_clean = leak.sub("", spec)
    return dup_title, f"{dup_title}\n\n{spec_clean}".strip(), len(spec) - len(spec_clean)


def dup_queries(data: str, items: dict) -> list[dict]:
    """Each pair queries with the duplicate's title and first spec version as filed. When the
    survivor changed after that moment, the expected hit is its filing-time copy, and its live
    version is excluded, so the query sees the corpus as it stood."""
    out = []
    with open(os.path.join(data, "asof.jsonl")) as f:
        pairs = [json.loads(line) for line in f]
    for p in pairs:
        dup = items[p["duplicate"]]
        title_q, spec_q, leak_removed = query_texts(p["dup_title"], p["dup_spec"], p["key"])
        expected, exclude, asof = p["key"], [dup.id], None
        if p["survivor_changed"]:
            expected, asof = f"{p['key']}@{dup.id}", dup.id
            exclude.append(p["key"])
        filt = {"kinds": ["issue"], "before": dup.created_at, "exclude": exclude, "asof": asof}
        base = {"expected": expected, "live_key": p["key"], "filter": filt, "source": dup.id,
                "parent": p.get("dup_parent"), "components": p.get("dup_components") or []}
        out.append({**base, "set": "dup-title", "id": f"{dup.id}:title", "text": title_q})
        out.append({**base, "set": "dup-spec", "id": f"{dup.id}:spec", "text": spec_q, "leak_removed": leak_removed})
    return out


def decision_queries(data: str, items: dict) -> list[dict]:
    """Questions are keyed by the ask id (or a unique prefix of it), so a fresh export still
    resolves them: {"questions_by_ask": {"<ask id prefix>": "<question>"}}."""
    with open(os.path.join(data, "..", "decision-questions.json")) as f:
        qs = json.load(f)["questions_by_ask"]
    asks = [i for i in items if i.startswith("ask:")]
    out = []
    for prefix, q in qs.items():
        match = [i for i in asks if i[4:].startswith(prefix)]
        if len(match) != 1:
            raise SystemExit(f"question key {prefix} matches {len(match)} asks")
        out.append({"set": "decision", "id": f"decision:{prefix}", "text": q, "expected": match[0],
                    "filter": {"kinds": ["issue", "ask"], "before": None, "exclude": [], "asof": None}, "source": match[0][4:]})
    return out


TIE_BAND = 0.01  # cosine: meaning scores this close count as a near-tie that keyword rank may break


def rrf_weighted(dense: list[str], kw: list[str], w: float) -> list[str]:
    score: dict[str, float] = {}
    for rank, iid in enumerate(dense, 1):
        score[iid] = score.get(iid, 0.0) + 1.0 / (RRF_K + rank)
    for rank, iid in enumerate(kw, 1):
        score[iid] = score.get(iid, 0.0) + w / (RRF_K + rank)
    return sorted(score, key=lambda k: (-score[k], k))


def tie_break(dense: list[str], dscore: dict[str, float], kw: list[str]) -> list[str]:
    """Meaning order; within a run of items whose scores are within TIE_BAND of the run's first,
    keyword rank decides (items keyword search did not return keep meaning order, after)."""
    kw_rank = {iid: r for r, iid in enumerate(kw)}
    pos = {iid: r for r, iid in enumerate(dense)}
    out, i = [], 0
    while i < len(dense):
        j = i
        while j < len(dense) and dscore[dense[i]] - dscore[dense[j]] <= TIE_BAND:
            j += 1
        out += sorted(dense[i:j], key=lambda iid: (kw_rank.get(iid, len(kw)), pos[iid]))
        i = j
    return out


def boost(dense: list[str], dscore: dict[str, float], items: dict, q: dict, beta: float) -> list[str]:
    """Add beta to the meaning score of a result sharing the query issue's parent or a component."""
    def meta(iid):
        raw = items[iid].raw
        if items[iid].asof_for:
            return raw.get("parent"), set(raw.get("components") or [])
        return raw.get("parent"), set((raw.get("components") or {}).get("ids") or [])
    qp, qc = q.get("parent"), set(q.get("components") or [])
    pos = {iid: r for r, iid in enumerate(dense)}
    def score(iid):
        p, c = meta(iid)
        return dscore[iid] + (beta if (qp and p == qp) or (qc & c) else 0.0)
    return sorted(dense, key=lambda iid: (-score(iid), pos[iid]))


def _where(filt: dict, alias: str = "i") -> tuple[str, dict]:
    parts = [f"{alias}.kind = any(%(kinds)s)", f"({alias}.asof_for is null or {alias}.asof_for = %(asof)s)"]
    params = {"kinds": filt["kinds"], "asof": filt["asof"]}
    if filt["before"]:
        parts.append(f"{alias}.created_at < %(before)s")
        params["before"] = filt["before"]
    if filt["exclude"]:
        parts.append(f"{alias}.id <> all(%(exclude)s)")
        params["exclude"] = filt["exclude"]
    return " and ".join(parts), params


def candidates(conn, filt) -> set[str]:
    w, p = _where(filt)
    return {r[0] for r in conn.execute(f"select id from items i where {w}", p)}


# ---------------------------------------------------------------------------------------------
# systems


def collapse_hits(results: list[dict], cands: set[str], alias: dict[str, str] | None = None) -> list[str]:
    """Dispatch search hits in order, collapsed to their issue (or ask) and kept only when that item
    is a candidate, each once."""
    ranked = []
    for r in results:
        if r["kind"] == "ask":
            iid = "ask:" + r["id"]
        elif r.get("issue"):
            iid = r["issue"]["key"]
        else:
            continue
        iid = (alias or {}).get(iid, iid)
        if iid in cands and iid not in ranked:
            ranked.append(iid)
    return ranked


def sys_production(text: str, cands: set[str], alias: dict[str, str]) -> tuple[list[str], float, str | None]:
    """Today's search sees only the live corpus, so a survivor's live key stands in for its
    filing-time copy; that gives production the post-consolidation text the others don't see."""
    t = time.time()
    try:
        res = dget.get("/search", {"q": text, "limit": 50})["results"]
    except Exception as e:  # e.g. a whole spec is too long for a URL
        return [], time.time() - t, str(e)[:200]
    return collapse_hits(res, cands, alias), time.time() - t, None


NEGATION = re.compile(r"(^|\s)-+(?=\w)")
# Fixed keyword search's query: the websearch terms joined with OR (websearch_to_tsquery ANDs them).
KW_TSQUERY = "replace(websearch_to_tsquery('english', %(q)s)::text, ' & ', ' | ')::tsquery"


def keyword_text(text: str) -> str:
    """A leading hyphen would negate a term in websearch syntax; a query quotes prose, not syntax."""
    return NEGATION.sub(r"\1", text)


def sys_keyword(conn, text: str, filt: dict, n: int = FUSE_DEPTH, norm: int = 1) -> tuple[list[str], float]:
    """norm is ts_rank_cd's normalisation flag: 1 divides by 1 + log(length), 2 by length."""
    w, p = _where(filt)
    t = time.time()
    rows = conn.execute(
        f"""with q as (select {KW_TSQUERY} as tsq)
            select i.id from items i, q where i.kw @@ q.tsq and {w}
             order by ts_rank_cd(i.kw, q.tsq, %(norm)s) desc, i.id limit %(n)s""",
        {**p, "q": keyword_text(text), "n": n, "norm": norm},
    ).fetchall()
    return [r[0] for r in rows], time.time() - t


def query_vector(conn, emb, text: str) -> tuple[np.ndarray, float]:
    h = hashlib.sha256(text.encode()).hexdigest()
    row = conn.execute("select v, latency from qcache where model = %s and text_hash = %s", (emb.name, h)).fetchone()
    if row:
        return np.asarray(row[0].to_numpy(), np.float32), row[1]  # an untyped vector column reads back as Vector
    t = time.time()
    v = emb.embed([text], "query")[0]
    dt = time.time() - t
    conn.execute("insert into qcache values (%s,%s,%s,%s) on conflict do nothing", (emb.name, h, v, dt))
    return v, dt


def sys_dense(conn, model: str, v: np.ndarray, filt: dict, n: int = FUSE_DEPTH) -> tuple[list[str], float, dict[str, float]]:
    w, p = _where(filt)
    t = time.time()
    rows = conn.execute(
        f"""select c.item_id, max(1 - (e.v <=> %(v)s)) as s from {slug(model)} e
              join chunks c on c.id = e.chunk_id join items i on i.id = c.item_id
             where {w} group by c.item_id order by s desc, c.item_id limit %(n)s""",
        {**p, "v": v, "n": n},
    ).fetchall()
    return [r[0] for r in rows], time.time() - t, {r[0]: r[1] for r in rows}


def rrf(*lists: list[str]) -> list[str]:
    score: dict[str, float] = {}
    for lst in lists:
        for rank, iid in enumerate(lst, 1):
            score[iid] = score.get(iid, 0.0) + 1.0 / (RRF_K + rank)
    return sorted(score, key=lambda k: (-score[k], k))


def passage_docs(rows) -> dict[str, str]:
    """Rows of (item, chunk text, similarity), sorted by item then similarity descending, become one
    reranker document per item: its best PASSAGES chunks, the first keeping its title line and later
    ones dropping the repeated title."""
    best: dict[str, list[str]] = {}
    for iid, text, _ in rows:
        if len(best.setdefault(iid, [])) < PASSAGES:
            best[iid].append(text)
    return {iid: "\n\n".join([texts[0]] + [t.split("\n", 1)[1] if "\n" in t else t for t in texts[1:]])
            for iid, texts in best.items()}


def passages(conn, model: str, v: np.ndarray, ids: list[str]) -> tuple[dict[str, str], float]:
    t = time.time()
    rows = conn.execute(
        f"""select c.item_id, c.text, 1 - (e.v <=> %(v)s) as s from {slug(model)} e join chunks c on c.id = e.chunk_id
             where c.item_id = any(%(ids)s) order by c.item_id, s desc""",
        {"v": v, "ids": ids},
    ).fetchall()
    return passage_docs(rows), time.time() - t


def rerank(conn, rr, query: str, ids: list[str], docs: dict[str, str]) -> tuple[list[str], float | None]:
    """Rerank ids by rr's scores. Scores are pointwise, so each (query, passage) score is cached
    and only unseen passages are sent. Returns latency only when the whole list was sent in one
    measured call; None when some scores came from the cache."""
    texts = [docs[i] for i in ids]
    key = hashlib.sha256(json.dumps([query, texts]).encode()).hexdigest()
    dkeys = [hashlib.sha256(f"{query}\0{t}".encode()).hexdigest() for t in texts]
    row = conn.execute("select scores, latency from rcache where reranker = %s and key = %s", (rr.name, key)).fetchone()
    if row:
        scores, dt = row
        with conn.cursor() as cur:
            cur.executemany("insert into rdoc values (%s,%s,%s) on conflict do nothing", [(rr.name, k, s) for k, s in zip(dkeys, scores)])
    else:
        cached = dict(conn.execute("select key, score from rdoc where reranker = %s and key = any(%s)", (rr.name, dkeys)).fetchall())
        missing = [j for j, k in enumerate(dkeys) if k not in cached]
        t = time.time()
        fresh = rr.rerank(query, [texts[j] for j in missing]) if missing else []
        dt = time.time() - t
        with conn.cursor() as cur:
            cur.executemany("insert into rdoc values (%s,%s,%s) on conflict do nothing", [(rr.name, dkeys[j], s) for j, s in zip(missing, fresh)])
        cached.update({dkeys[j]: s for j, s in zip(missing, fresh)})
        scores = [cached[k] for k in dkeys]
        if len(missing) == len(texts):
            conn.execute("insert into rcache values (%s,%s,%s,%s) on conflict do nothing", (rr.name, key, scores, dt))
        else:
            dt = None
    order = sorted(range(len(ids)), key=lambda k: (-scores[k], k))
    return [ids[k] for k in order], dt


# ---------------------------------------------------------------------------------------------
# run


def rank_of(ranked: list[str], expected: str) -> int | None:
    try:
        r = ranked.index(expected) + 1
    except ValueError:
        return None
    return r if r <= DEPTH else None


def cmd_run(args):
    items, _ = corpus.load(args.data)
    conn = connect()
    decisions = decision_queries(args.data, items) if "decision" in args.sets else []
    queries = [q for q in dup_queries(args.data, items) + decisions if q["set"] in args.sets]
    embedders = {m: P.EMBEDDERS[m]() for m in args.embedders}
    rerankers = {r: P.RERANKERS[r]() for r in args.rerankers}
    pairs = [tuple(p.split("+")) for p in args.pairs] if args.pairs else [(e, r) for e in embedders for r in rerankers]
    results = json.load(open(args.out)) if os.path.exists(args.out) else {"queries": {}}
    for q in queries:
        rec = results["queries"].setdefault(q["id"], {"set": q["set"], "expected": q["expected"], "systems": {}})
        cands = candidates(conn, q["filter"])
        rec["findable"] = q["expected"] in cands
        rec["candidates"] = len(cands)
        S = rec["systems"]

        def put(name, ranked, latency, **extra):
            S[name] = {"rank": rank_of(ranked, q["expected"]), "latency": latency, "top": ranked[:10], **extra}

        if args.production and "A" not in S:
            alias = {q["live_key"]: q["expected"]} if q.get("live_key") else {}
            ranked, dt, err = sys_production(q["text"], cands, alias)
            put("A", ranked, dt, error=err, returned=len(ranked))
        kw, kw_dt = sys_keyword(conn, q["text"], q["filter"])
        put("B", kw, kw_dt, returned=len(kw))
        kw_len, kw_len_dt = sys_keyword(conn, q["text"], q["filter"], norm=2)
        put("B:len", kw_len, kw_len_dt, returned=len(kw_len))
        for m, emb in embedders.items():
            v, q_dt = query_vector(conn, emb, q["text"])
            dense, d_dt, dscore = sys_dense(conn, m, v, q["filter"])
            put(f"C:{m}", dense, q_dt + d_dt)
            fused = rrf(kw, dense)
            put(f"D:{m}", fused, q_dt + d_dt + kw_dt, recall_at_rerank_depth=q["expected"] in fused[: args.rerank_depth])
            for wt in (0.25, 0.5, 0.75):
                put(f"W{wt}:{m}", rrf_weighted(dense, kw, wt), q_dt + d_dt + kw_dt)
            put(f"T:{m}", tie_break(dense, dscore, kw), q_dt + d_dt + kw_dt)
            for beta in (0.02, 0.05, 0.1):
                # Questions have no component or parent, so the boost leaves their meaning order as is.
                boosted = dense if q["set"] == "decision" else boost(dense, dscore, items, q, beta)
                put(f"P{beta}:{m}", boosted, q_dt + d_dt)
            for em, rn in pairs:
                if em != m:
                    continue
                # E reranks the fused list (the design); F reranks the dense list alone.
                base_list = fused if args.rerank_input == "fused" else dense
                top = base_list[: args.rerank_depth]
                docs, p_dt = passages(conn, m, v, top)
                reranked, r_dt = rerank(conn, rerankers[rn], q["text"], top, docs)
                prefix = "E" if args.rerank_input == "fused" else "F"
                name = f"{prefix}:{m}+{rn}" + ("" if args.rerank_depth == RERANK_DEPTH else f"@{args.rerank_depth}")
                base = q_dt + d_dt + p_dt + (kw_dt if prefix == "E" else 0.0)
                # Items below the rerank depth keep their first-stage order after the reranked ones.
                put(name, reranked + base_list[args.rerank_depth :], None if r_dt is None else base + r_dt,
                    base_latency=base, rerank_latency=r_dt)
                if prefix == "E" and args.rerank_depth == RERANK_DEPTH:
                    # Q: rerank question-style searches only; duplicate searches keep meaning order.
                    if q["set"] == "decision":
                        put(f"Q:{m}+{rn}", reranked + base_list[args.rerank_depth :], None if r_dt is None else base + r_dt,
                            base_latency=base, rerank_latency=r_dt)
                    else:
                        put(f"Q:{m}+{rn}", dense, q_dt + d_dt)
        with open(args.out, "w") as f:
            json.dump(results, f)
        print(q["id"], {k: v["rank"] for k, v in S.items()}, flush=True)
    log_usage(args.data, f"run {os.path.basename(args.out)}")


def log_usage(data: str, label: str) -> None:
    """Append this process's provider usage to <data>/../usage.jsonl, the spend ledger."""
    usage = {k: dict(v) for k, v in P.USAGE.items()}
    print(json.dumps(usage))
    with open(os.path.join(data, "..", "usage.jsonl"), "a") as f:
        f.write(json.dumps({"at": time.time(), "label": label, "usage": usage}) + "\n")


# ---------------------------------------------------------------------------------------------
# report


def metrics(ranks: list[int | None]) -> dict:
    n = len(ranks)
    return {
        "n": n,
        "r5": sum(1 for r in ranks if r and r <= 5) / n,
        "r10": sum(1 for r in ranks if r and r <= 10) / n,
        "mrr": sum(1 / r for r in ranks if r) / n,
        "ndcg10": sum(1 / math.log2(r + 1) for r in ranks if r and r <= 10) / n,
    }


# USD list prices. None marks a price not published where we could read it; the report prices those
# at the stated assumption and says so.
PRICES = {
    "bedrock:us.cohere.embed-v4:0": ("input_tokens", 0.12e-6, None),
    "bedrock:amazon.titan-embed-text-v2:0": ("input_tokens", 0.02e-6, None),
    "gemini:gemini-embedding-001": ("input_tokens_estimated", 0.15e-6, None),
    "openai:text-embedding-3-large": ("input_tokens", 0.13e-6, None),
    "cohere:embed-v5.0-pro": ("input_tokens", 0.12e-6, "assumed at embed-v4's price"),
    "cohere:embed-v5.0-fast": ("input_tokens", 0.12e-6, "assumed at embed-v4's price"),
    "voyage:voyage-4-large": ("input_tokens", 0.12e-6, None),
    "voyage:voyage-4": ("input_tokens", 0.06e-6, None),
    # Bedrock rerank bills per 100 document chunks; a chunk is estimated as 2,048 characters (512 tokens).
    "bedrock:cohere.rerank-v3-5:0": ("bedrock_chunk_units", 2.00e-3, None),
    "bedrock:amazon.rerank-v1:0": ("bedrock_chunk_units", 1.00e-3, None),
    "cohere:rerank-v4.0-pro": ("search_units", 5.00e-3, "assumed at $5 per 1,000 searches"),
    "cohere:rerank-v4.0-fast": ("search_units", 2.00e-3, "assumed at rerank-v3.5's $2 per 1,000 searches"),
    "voyage:rerank-2.5": ("tokens", 0.05e-6, None),
    "voyage:rerank-2.5-lite": ("tokens", 0.02e-6, None),
}


def spend(data: str) -> list[tuple[str, dict, float, str | None]]:
    total: dict[str, dict[str, float]] = {}
    path = os.path.join(data, "..", "usage.jsonl")
    if os.path.exists(path):
        for line in open(path):
            for prov, u in json.loads(line)["usage"].items():
                t = total.setdefault(prov, {})
                for k, v in u.items():
                    t[k] = t.get(k, 0.0) + v
    rows = []
    for prov, u in sorted(total.items()):
        if prov.startswith("bedrock:") and "pair_chars" in u:
            u["bedrock_chunk_units"] = max(u.get("calls", 0), u["pair_chars"] / 2048 / 100)
        unit, price, note = PRICES.get(prov, (None, 0.0, "local, no charge"))
        rows.append((prov, u, u.get(unit, 0.0) * price if unit else 0.0, note))
    return rows


def load_results(paths: list[str]) -> dict:
    """Merge result files from parallel runs: each query's systems are unioned."""
    merged: dict = {}
    for p in paths:
        for qid, q in json.load(open(p))["queries"].items():
            m = merged.setdefault(qid, {**q, "systems": {}})
            m["systems"].update(q["systems"])
    return merged


def rr(rank):
    return 1 / rank if rank else 0.0


def latency(q: dict, s: str) -> float:
    """A system's latency on one query. When the reranker's scores came partly from the cache,
    the rerank time is taken from the sibling system (E for F, F for E) that sent the full list,
    else from the median full call of that reranker."""
    r = q["systems"][s]
    if r["latency"] is not None:
        return r["latency"]
    sibling = q["systems"].get(("F" if s[0] == "E" else "E") + s[1:])
    if sibling and sibling.get("rerank_latency") is not None:
        return r["base_latency"] + sibling["rerank_latency"]
    rn = s.split("+", 1)[1]
    measured = [x["rerank_latency"] for x in q["systems"].values() if x.get("rerank_latency") is not None]
    same = [v["rerank_latency"] for k, v in q["systems"].items() if k.endswith("+" + rn) and v.get("rerank_latency") is not None]
    return r["base_latency"] + statistics.median(same or measured)


def cmd_report(args):
    res = load_results(args.out)
    sets = ["dup-title", "dup-spec", "decision"]
    systems = sorted({s for q in res.values() for s in q["systems"]}, key=lambda s: (s[0], s))
    complete = [s for s in systems if all(s in q["systems"] for q in res.values())]
    table = {}
    for s in complete:
        table[s] = {}
        for st in sets:
            rows = [q["systems"][s] for q in res.values() if q["set"] == st]
            lats = [latency(q, s) for q in res.values() if q["set"] == st]
            table[s][st] = {**metrics([r["rank"] for r in rows]), "lat": statistics.median(lats)}
        table[s]["score"] = statistics.mean(table[s][st]["mrr"] for st in sets)
        table[s]["lat"] = statistics.median(latency(q, s) for q in res.values())
    ranked = sorted(complete, key=lambda s: -table[s]["score"])

    print("## Summary: every system, ranked by mean MRR over the three query sets\n")
    print("| system | score (mean MRR) | dup title R@5 | dup title MRR | dup spec R@5 | dup spec MRR | decision R@5 | decision MRR | median latency (s) |")
    print("|---|---|---|---|---|---|---|---|---|")
    for s in ranked:
        t = table[s]
        cells = " | ".join(f"{t[st]['r5']:.2f} | {t[st]['mrr']:.3f}" for st in sets)
        print(f"| {s} | {t['score']:.3f} | {cells} | {t['lat']:.2f} |")

    for st in sets:
        qs = [q for q in res.values() if q["set"] == st]
        print(f"\n## {st}: {len(qs)} queries, {sum(q['findable'] for q in qs)} findable at all\n")
        print("| system | recall@5 | recall@10 | MRR | nDCG@10 | median latency (s) |")
        print("|---|---|---|---|---|---|")
        for s in ranked:
            m = table[s][st]
            print(f"| {s} | {m['r5']:.2f} | {m['r10']:.2f} | {m['mrr']:.3f} | {m['ndcg10']:.3f} | {m['lat']:.2f} |")

    fused = [s for s in ranked if s.startswith("D:")]
    if fused:
        print(f"\n## Reranker ceiling: share of queries whose expected item is in the fused top {RERANK_DEPTH}\n")
        print("| fused system | " + " | ".join(sets) + " |")
        print("|---|" + "---|" * len(sets))
        for s in fused:
            cells = []
            for st in sets:
                qs = [q for q in res.values() if q["set"] == st]
                cells.append(f"{sum(q['systems'][s].get('recall_at_rerank_depth', False) for q in qs)}/{len(qs)}")
            print(f"| {s} | " + " | ".join(cells) + " |")

    comparisons = [(ranked[0], ranked[1])] if len(ranked) >= 2 else []
    comparisons += [tuple(c.split(",")) for c in args.compare or []]
    for w, r in comparisons:
        compare(res, table, sets, w, r)

    rows = spend(args.data) if args.data else []
    if rows:
        print("\n## Spend (list prices)\n")
        print("| provider:model | usage | estimated USD | note |")
        print("|---|---|---|---|")
        for prov, u, usd, note in rows:
            use = ", ".join(f"{k}={v:,.0f}" for k, v in u.items())
            print(f"| {prov} | {use} | {usd:.2f} | {note or ''} |")
        print(f"| total | | {sum(r[2] for r in rows):.2f} | |")


def compare(res: dict, table: dict, sets: list[str], w: str, r: str) -> None:
    """Paired comparison on reciprocal rank: which queries the margin comes from, and a paired
    bootstrap over queries (resampled within each set, the three set means averaged)."""
    print(f"\n## {w} vs {r}\n")
    rng = np.random.default_rng(0)
    per_set = {}
    for st in sets:
        qs = [(qid, q) for qid, q in res.items() if q["set"] == st]
        d = np.array([rr(q["systems"][w]["rank"]) - rr(q["systems"][r]["rank"]) for _, q in qs])
        per_set[st] = d
        wins, losses = int((d > 0).sum()), int((d < 0).sum())
        diff = ", ".join(f"{qid} {q['systems'][w]['rank']} vs {q['systems'][r]['rank']}" for qid, q in qs if q["systems"][w]["rank"] != q["systems"][r]["rank"])
        print(f"- {st}: {w} ranks the expected item higher on {wins} queries, {r} on {losses}, tied on {len(d) - wins - losses}; MRR difference {d.mean():+.3f}. Differing ranks: {diff or 'none'}")
    samples = np.array([np.mean([rng.choice(d, len(d)).mean() for d in per_set.values()]) for _ in range(10000)])
    lo, hi = np.percentile(samples, [2.5, 97.5])
    print(f"- score difference {table[w]['score'] - table[r]['score']:+.3f}; paired bootstrap 95% interval [{lo:+.3f}, {hi:+.3f}]; share of resamples where {w} is not ahead: {(samples <= 0).mean():.2f}")


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    a = sub.add_parser("load")
    a.add_argument("--data", required=True)
    a = sub.add_parser("embed")
    a.add_argument("--data", required=True)
    a.add_argument("--model", required=True, choices=list(P.EMBEDDERS))
    a.add_argument("--step", type=int, default=960)
    a.add_argument("--shard", default="0/1", help="i/n: embed only this share of the chunks, for parallel local runs")
    a = sub.add_parser("run")
    a.add_argument("--data", required=True)
    a.add_argument("--out", required=True)
    a.add_argument("--sets", nargs="+", default=["dup-title", "dup-spec", "decision"])
    a.add_argument("--embedders", nargs="*", default=[], choices=list(P.EMBEDDERS))
    a.add_argument("--rerankers", nargs="*", default=[], choices=list(P.RERANKERS))
    a.add_argument("--pairs", nargs="*", help="embedder+reranker pairs; default every combination")
    a.add_argument("--production", action="store_true", help="also query today's production search (GET only)")
    a.add_argument("--rerank-depth", type=int, default=RERANK_DEPTH, help="how many first-stage items the reranker sees")
    a.add_argument("--rerank-input", choices=["fused", "dense"], default="fused", help="rerank the fused list (E) or the dense list (F)")
    a = sub.add_parser("report")
    a.add_argument("--out", required=True, nargs="+", help="one or more result files, merged")
    a.add_argument("--data", help="data directory, to price the spend ledger beside it")
    a.add_argument("--compare", nargs="*", help="extra system pairs to compare, as 'sysA,sysB'")
    args = ap.parse_args()
    os.umask(0o077)
    {"load": cmd_load, "embed": cmd_embed, "run": cmd_run, "report": cmd_report}[args.cmd](args)


if __name__ == "__main__":
    main()

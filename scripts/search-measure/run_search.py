#!/usr/bin/env python3
"""Run the cases against a Dispatch search route and record where the right answer landed.

usage: run_search.py --cases cases.json --fixture-dsn <dsn> --url http://127.0.0.1:8799 \\
         --token <bearer> --out $TMPDIR/out.jsonl [--only serial120.json] [--workers 1]

Each case's query is read back out of the fixture the server is serving (case_text.py) and
checked against the descriptor's SHA-256: a case whose text has moved is refused, never guessed
at and never run. The queries this run sent are never written to the output; only ranks are, so
a row of the output carries no Dispatch text.

One request at a time (--workers 1) is the latency measurement: a person's search is served
alone. More workers measure recall faster and say nothing about latency.
"""

import argparse
import json
import sys
import threading
import time
import urllib.parse

import pg
from case_text import digest, extract_ask_span, extract_document_span
from dispatch_read import ReadOnlyDispatch, pmap


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cases", required=True)
    parser.add_argument("--fixture-dsn", required=True)
    parser.add_argument("--url", required=True, help="the search server, built from this tree")
    parser.add_argument("--token", default="", help="bearer; else DISPATCH_TOKEN_FILE's contents")
    parser.add_argument("--out", required=True)
    parser.add_argument("--only", default="", help="a JSON list of case ids to run")
    parser.add_argument("--side", default="", help="the side file build_cases.py wrote")
    parser.add_argument("--workers", type=int, default=1)
    parser.add_argument("--limit", type=int, default=50, help="results per page; a rank past it is absent")
    parser.add_argument("--first-words", type=int, default=0,
                        help="send only each query's first N words: the probe of what a query's "
                             "length costs, since a short query is likelier to match as typed and "
                             "a long one likelier to answer nothing and reach the rarest-words leg")
    parser.add_argument("--warmup", action="store_true",
                        help="run every case once, unrecorded, before the run that is recorded")
    arguments = parser.parse_args()

    with open(arguments.cases) as handle:
        cases = json.load(handle)
    if arguments.only:
        with open(arguments.only) as handle:
            wanted = set(json.load(handle))
        cases = [case for case in cases if case["case_id"] in wanted]
    side = {}
    if arguments.side:
        with open(arguments.side) as handle:
            side = json.load(handle)

    queries, refused = resolve(cases, arguments.fixture_dsn, side)
    print(f"{len(queries)} cases resolved, {len(refused)} refused", file=sys.stderr)
    for case_id, why in refused[:10]:
        print(f"  refused {case_id}: {why}", file=sys.stderr)
    runnable = [case for case in cases if case["case_id"] in queries]
    if arguments.first_words:
        queries = {case_id: " ".join(query.split()[: arguments.first_words])
                   for case_id, query in queries.items()}

    dispatch = ReadOnlyDispatch(arguments.url, arguments.token or None)
    limit = arguments.limit
    if arguments.warmup:
        print("warming up", file=sys.stderr)
        for case in runnable:
            measure(dispatch, case, queries[case["case_id"]], limit)
    # Each row is written as its search answers, so a run that is stopped keeps what it measured
    # and a long run can be read while it is still going.
    handle = open(arguments.out, "w")
    lock = threading.Lock()
    written = [0]

    def run(case):
        try:
            row = measure(dispatch, case, queries[case["case_id"]], limit)
        except Exception as error:  # noqa: BLE001 - one unanswered search is one row, not the run
            row = {"case_id": case["case_id"], "kind": case["kind"], "error": repr(error)}
        with lock:
            handle.write(json.dumps(row) + "\n")
            handle.flush()
            written[0] += 1
        return None

    pmap(run, runnable, max(1, arguments.workers), "searches", every=50)
    for case_id, why in refused:
        handle.write(json.dumps({"case_id": case_id, "refused": why}) + "\n")
    handle.close()
    print(f"{written[0]} runs -> {arguments.out}", file=sys.stderr)


def resolve(cases, dsn, side):
    """Every case's query, read from the fixture and checked against its hash."""
    documents = {}
    wanted_documents = sorted({
        case["source"]["artifact_id"] for case in cases if case["source"]["kind"] == "document"
    })
    if wanted_documents:
        for row in pg.rows(dsn, """
            select v.artifact_id::text as artifact_id, v.number, v.markdown
              from artifact_versions v
             where v.artifact_id = any(%s::uuid[])""" % literal(wanted_documents)):
            documents[(row["artifact_id"], row["number"])] = row["markdown"]
    asks = {}
    wanted_asks = sorted({case["source"]["ask_id"] for case in cases if case["source"]["kind"] == "ask"})
    if wanted_asks:
        for row in pg.rows(dsn, """
            select k.id::text as id, k.question
              from asks k
             where k.id = any(%s::uuid[])""" % literal(wanted_asks)):
            asks[row["id"]] = row["question"]

    queries, refused = {}, []
    for case in cases:
        source = case["source"]
        if source["kind"] == "document":
            markdown = documents.get((source["artifact_id"], source["version"]))
            if markdown is None:
                refused.append((case["case_id"], "the fixture holds no such document version"))
                continue
            query = extract_document_span(markdown, source["offset"], source["length"])
        elif source["kind"] == "ask":
            question = asks.get(source["ask_id"])
            if question is None:
                refused.append((case["case_id"], "the fixture holds no such ask"))
                continue
            query = extract_ask_span(question, source["offset"], source["length"])
        else:
            query = side.get(case["sha256"])
            if query is None:
                refused.append((case["case_id"], "no side file holds this query"))
                continue
        if digest(query) != case["sha256"]:
            refused.append((case["case_id"], "the text has moved since the descriptor was built"))
            continue
        queries[case["case_id"]] = query
    return queries, refused


def literal(values):
    """A uuid[] literal of values already checked to be uuids by the route that answered them."""
    for value in values:
        if not all(character in "0123456789abcdef-" for character in value.lower()):
            raise SystemExit(f"not a uuid: {value!r}")
    return "'{" + ",".join(values) + "}'"


def measure(dispatch, case, query, limit):
    """One search, as a person's own browser makes it."""
    path = "/api/v1/search?" + urllib.parse.urlencode({"q": query, "limit": limit})
    started = time.monotonic()
    answer = dispatch.get(path, tries=2)
    wall = (time.monotonic() - started) * 1000
    results = answer.get("results") or []
    row = {
        "case_id": case["case_id"],
        "kind": case["kind"],
        "answer_kind": case["answer_kind"],
        "answer_id": case["answer_id"],
        "issue": case.get("issue"),
        "project": case.get("project"),
        "nwords": len(query.split()),
        "rank": rank_of(results, case["answer_kind"], case["answer_id"]),
        "total": answer.get("total"),
        "took_ms": answer.get("took_ms"),
        "wall_ms": round(wall, 1),
        "degraded": answer.get("degraded"),
        # Whether the any-word leg ran: the server's own reading of the words it was sent.
        "broadened": (answer.get("query") or {}).get("broadened"),
        # kind and id alone: a row of this file carries no Dispatch text.
        "top": [{"kind": result.get("kind"), "id": str(result.get("id"))} for result in results[:5]],
    }
    if case.get("doc_artifact_id"):
        row["doc_rank"] = rank_of(results, "document", case["doc_artifact_id"])
    if case.get("issue"):
        row["any_item_of_issue_rank"] = next(
            (index for index, result in enumerate(results, 1)
             if (result.get("owner") or {}).get("key") == case["issue"]
             or result.get("id") == case["issue"]),
            None,
        )
    return row


def rank_of(results, kind, identifier):
    for index, result in enumerate(results, 1):
        if result.get("kind") == kind and str(result.get("id")) == str(identifier):
            return index
    return None


if __name__ == "__main__":
    main()

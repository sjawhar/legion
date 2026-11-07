#!/usr/bin/env python3
"""Turn a set of measured cases into descriptors this repository can carry.

usage: build_cases.py --corpus $TMPDIR/<dir> --cases <cases.json> --out cases.json

A case the baseline measured is a query and the item that answers it. The query is Dispatch's own
text, so this repository holds no case's text: for each case the output names the item the text
comes from, the version, the character range, and the SHA-256 of the text that range yields
(case_text.py). The runner reads the range back out of the fixture and refuses a case whose hash
has moved, so a case always runs the words it was measured with or does not run at all.

A case whose query is nobody's text - a reworded title, written to ask search the same question
in other words - cannot be described at all: it has no item, no range and no hash to re-extract,
and committing the paraphrase itself would put Dispatch's own content in a public repository,
since a reworded title still names an issue's subject. Such a case is counted and left out.
"""

import argparse
import json
import sys
from collections import Counter

from case_text import digest, document_sentence_spans, extract_ask_span, extract_document_span
from corpus import read_jsonl


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--corpus", required=True)
    parser.add_argument("--cases", required=True, help="the measured cases, each with its query")
    parser.add_argument("--out", required=True, help="the descriptors, safe to commit")
    arguments = parser.parse_args()

    documents = {row["artifact_id"]: row for row in read_jsonl(arguments.corpus, "documents.jsonl")}
    asks = {row["id"]: row for row in read_jsonl(arguments.corpus, "asks.jsonl")}
    with open(arguments.cases) as handle:
        cases = json.load(handle)

    descriptors, outcomes = [], Counter()
    unexpressed = []
    for case in cases:
        descriptor, outcome = describe(case, documents, asks)
        outcomes[outcome] += 1
        if descriptor is None:
            unexpressed.append((case["case_id"], outcome))
            continue
        descriptors.append(descriptor)

    with open(arguments.out, "w") as handle:
        # Two spaces and a trailing newline: the repository's JSON formatter (`bunx biome check`)
        # reads this file too.
        json.dump(descriptors, handle, indent=2)
        handle.write("\n")

    print(f"{len(descriptors)}/{len(cases)} cases described -> {arguments.out}")
    for outcome, count in sorted(outcomes.items()):
        print(f"  {outcome}: {count}")
    if unexpressed:
        print("  not described: " + ", ".join(f"{case_id} ({why})" for case_id, why in unexpressed[:20]), file=sys.stderr)


def describe(case, documents, asks):
    """One case as a descriptor, with the word for what became of it."""
    common = {
        "case_id": case["case_id"],
        "kind": case["kind"],
        "answer_kind": case["answer_kind"],
        "answer_id": case["answer_id"],
        "issue": case.get("issue"),
        "project": case.get("project"),
        "sha256": digest(case["query"]),
    }
    if case["kind"] == 1:
        common["doc_artifact_id"] = case.get("doc_artifact_id")
        document = documents.get(case.get("doc_artifact_id"))
        if document is None:
            return None, "document not harvested"
        span = find_document_span(document["markdown"], case["query"])
        if span is None:
            return None, "sentence no longer in the document"
        offset, length = span
        common["source"] = {
            "kind": "document",
            "artifact_id": document["artifact_id"],
            "version": document["version"],
            "offset": offset,
            "length": length,
        }
        return common, "described from a document"
    if case["kind"] == 3:
        ask = asks.get(case["answer_id"])
        if ask is None:
            return None, "ask not harvested"
        length = find_ask_prefix(ask["question"], case["query"])
        if length is None:
            return None, "question no longer yields the query"
        common["source"] = {"kind": "ask", "ask_id": ask["id"], "offset": 0, "length": length}
        return common, "described from an ask"
    # Kind 2, a reworded title: a paraphrase is written text no document holds, so there is no
    # item to name, no range to read back and nothing to re-extract, and the paraphrase itself
    # names an issue's subject, which this public repository may not carry.
    return None, "a reworded title is nobody's text: not measurable here"


def find_document_span(markdown, query):
    """The range of markdown whose extraction is query, if the document still holds it."""
    for offset, length, sentence in document_sentence_spans(markdown):
        if sentence == query and extract_document_span(markdown, offset, length) == query:
            return offset, length
    return None


def find_ask_prefix(question, query):
    """The shortest prefix of question whose extraction is query.

    A long question was cut to length after it was cleaned, so the cut lands wherever the markup
    put it: every prefix is tried, from the shortest that could hold the query, until the
    extraction has grown past it.
    """
    if extract_ask_span(question, 0, len(question)) == query:
        return len(question)
    for length in range(len(query), len(question) + 1):
        extracted = extract_ask_span(question, 0, length)
        if extracted == query:
            return length
        if len(extracted) > len(query) + 32:
            return None
    return None


if __name__ == "__main__":
    main()

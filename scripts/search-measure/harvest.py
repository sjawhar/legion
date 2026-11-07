#!/usr/bin/env python3
"""Harvest a Dispatch server's searchable content through its read API. GET only.

usage: harvest.py --out $TMPDIR/<dir> [--workers 16] [--projects KEY,KEY] [--issue-limit N]

It writes, into --out:

  projects.json    every project's key and name
  issues.jsonl     every issue: key, title, status, labels, rank, assignee, updated_at, parent
  documents.jsonl  every document with its latest version's markdown and version number
  comments.jsonl   every comment of every issue, and of every project document
  asks.jsonl       every ask of every issue, and of every project document
  messages.jsonl   every message of every issue, replies flattened
  coverage.json    the server's own item counts per kind, for comparison with the fixture's

Nothing here writes: every call is a GET, and a harvest of production is a read of production.
"""

import argparse
import os

from corpus import say, write_json, write_jsonl
from dispatch_read import DispatchRefused, ReadOnlyDispatch, pmap, require_under_tmpdir


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, help="corpus directory, under $TMPDIR")
    parser.add_argument("--workers", type=int, default=16)
    parser.add_argument("--projects", default="", help="comma-separated project keys; default all")
    parser.add_argument("--issue-limit", type=int, default=0, help="harvest at most N issues per project (0: all)")
    arguments = parser.parse_args()

    out = require_under_tmpdir(arguments.out)
    os.makedirs(out, exist_ok=True)
    dispatch = ReadOnlyDispatch()
    workers = arguments.workers

    projects = dispatch.get("/api/v1/projects")
    wanted = {key for key in arguments.projects.split(",") if key}
    if wanted:
        projects = [project for project in projects if project["key"] in wanted]
    write_json(out, "projects.json", [{"key": p["key"], "name": p["name"]} for p in projects])
    say(f"{len(projects)} projects")

    issues = []
    for project in projects:
        rows = dispatch.paged(f"/api/v1/issues?project={project['key']}", "issues")
        if arguments.issue_limit:
            rows = sorted(rows, key=lambda row: row["key"])[: arguments.issue_limit]
        issues.extend(rows)
    write_jsonl(out, "issues.jsonl", (issue_row(issue) for issue in issues))
    say(f"{len(issues)} issues")

    keys = [issue["key"] for issue in issues]
    loaded = set(keys)

    # Every artifact of a project, in one list per project: the route answers an issue's
    # documents as well as the project's own, so a corpus of 13,000 documents costs one request
    # per project rather than one per issue.
    artifacts, seen = [], set()
    project_lists = pmap(
        lambda key: dispatch.get(f"/api/v1/projects/{key}/artifacts"),
        [project["key"] for project in projects],
        workers,
    )
    for project, answer in zip(projects, project_lists):
        if isinstance(answer, Exception):
            say(f"  project {project['key']} artifacts unread: {answer!r}")
            continue
        for artifact in answer:
            if artifact["kind"] != "doc" or not artifact.get("versions"):
                continue
            if artifact.get("issue_key") and artifact["issue_key"] not in loaded:
                continue
            artifacts.append(artifact)
            seen.add(artifact["id"])
    say(f"{len(artifacts)} documents from the project lists")

    # An issue's primary document - its spec, the one document a search for a sentence of a spec
    # has to find - is the one artifact that list leaves out (`and not is_primary`), so it is
    # read from the issue's own list.
    with_primary = [issue["key"] for issue in issues if issue.get("primary_artifact_id")]
    issue_lists = pmap(
        lambda key: dispatch.get(f"/api/v1/issues/{key}/artifacts"), with_primary, workers, "primary documents", every=1000
    )
    unread_primaries = 0
    for key, answer in zip(with_primary, issue_lists):
        if isinstance(answer, Exception):
            unread_primaries += 1
            continue
        for artifact in answer:
            if (artifact.get("primary") and artifact["kind"] == "doc"
                    and artifact.get("versions") and artifact["id"] not in seen):
                artifacts.append(artifact)
                seen.add(artifact["id"])
    say(f"{len(artifacts)} documents, {unread_primaries} issues' primary unread")

    def text_of(artifact):
        if artifact.get("issue_key"):
            path = f"/api/v1/issues/{artifact['issue_key']}/artifacts/{artifact['slug']}/text"
        else:
            path = f"/api/v1/projects/{artifact['project']}/artifacts/{artifact['slug']}/text"
        return dispatch.get(path)

    texts = pmap(text_of, artifacts, workers, "document text", every=1000)
    documents, unread = [], 0
    for artifact, text in zip(artifacts, texts):
        if isinstance(text, Exception):
            unread += 1
            continue
        documents.append(document_row(artifact, text))
    write_jsonl(out, "documents.jsonl", documents)
    say(f"{len(documents)} documents with text, {unread} unread")
    standalone = [document for document in documents if document["issue_key"] is None]

    comments, asks, messages = [], [], []

    def issue_items(key):
        return (
            dispatch.get(f"/api/v1/issues/{key}/comments"),
            dispatch.get(f"/api/v1/issues/{key}/asks"),
            dispatch.get(f"/api/v1/issues/{key}/messages"),
        )

    unread_items = 0
    for key, answer in zip(keys, pmap(issue_items, keys, workers, "issue items")):
        if isinstance(answer, Exception):
            unread_items += 1
            continue
        issue_comments, issue_asks, issue_messages = answer
        comments.extend(comment_row(comment, key, None) for comment in issue_comments)
        asks.extend(ask_row(ask, key, None) for ask in issue_asks)
        flatten_threads(issue_messages, key, messages)

    def document_items(artifact_id):
        try:
            return (
                dispatch.get(f"/api/v1/artifacts/{artifact_id}/comments"),
                dispatch.get(f"/api/v1/artifacts/{artifact_id}/asks"),
            )
        except DispatchRefused as refused:
            return refused

    standalone_ids = [document["artifact_id"] for document in standalone]
    for artifact_id, answer in zip(standalone_ids, pmap(document_items, standalone_ids, workers)):
        if isinstance(answer, Exception):
            continue
        document_comments, document_asks = answer
        comments.extend(comment_row(comment, None, artifact_id) for comment in document_comments)
        asks.extend(ask_row(ask, None, artifact_id) for ask in document_asks)

    write_jsonl(out, "comments.jsonl", comments)
    write_jsonl(out, "asks.jsonl", asks)
    write_jsonl(out, "messages.jsonl", messages)
    say(f"{len(comments)} comments, {len(asks)} asks, {len(messages)} messages, {unread_items} issues unread")

    try:
        write_json(out, "coverage.json", counts_only(dispatch.get("/api/v1/search/coverage")))
    except Exception as error:  # noqa: BLE001 - a coverage read is a comparison, never the harvest
        say(f"coverage unread: {error!r}")


def issue_row(issue):
    key = issue["key"]
    project, number = key.rsplit("-", 1)
    return {
        "key": key,
        "project": project,
        "number": int(number),
        "title": issue["title"],
        "status": issue["status"],
        "labels": issue.get("labels") or [],
        "rank": issue.get("rank") or "U",
        "assignee": issue.get("assignee"),
        "parent": issue.get("parent"),
        "updated_at": issue["updated_at"],
        "primary_artifact_id": issue.get("primary_artifact_id"),
    }


def document_row(artifact, text):
    """One document row: its latest version's number as the text route answers it, with that
    version's own metadata from the artifact's version list when it carries it."""
    number = text.get("version") or max(version["number"] for version in artifact["versions"])
    version = next((v for v in artifact["versions"] if v["number"] == number), None) or {}
    return {
        "artifact_id": artifact["id"],
        "issue_key": artifact.get("issue_key"),
        "project": artifact["project"],
        "slug": artifact["slug"],
        "name": artifact["name"],
        "primary": bool(artifact.get("primary")),
        "created_at": artifact["created_at"],
        "version": number,
        "version_created_at": version.get("created_at") or artifact["created_at"],
        "version_named": bool(version.get("named")),
        "version_summary": version.get("summary"),
        "markdown": text.get("markdown") or "",
    }


def comment_row(comment, issue_key, artifact_id):
    return {
        "id": comment["id"],
        "issue_key": issue_key,
        "artifact_id": artifact_id,
        "body": comment["body"],
        "created_at": comment["created_at"],
        "resolved": bool(comment.get("resolved")),
        "reply_to": comment.get("reply_to"),
        "ask_id": comment.get("ask_id"),
    }


def ask_row(ask, issue_key, artifact_id):
    return {
        "id": ask["id"],
        "issue_key": issue_key,
        "artifact_id": artifact_id,
        "question": ask["question"],
        "options": ask.get("options") or [],
        "answer": ask.get("answer"),
        "state": ask.get("state") or "open",
        "created_at": ask["created_at"],
    }


def flatten_threads(threads, issue_key, into):
    """Every message of an issue's threads, the replies of each thread included. A root is a node
    carrying its message under `message`; a reply is the message itself."""
    for node in threads:
        message = node.get("message", node)
        into.append(
            {
                "id": message["id"],
                "issue_key": issue_key,
                "body": message["body"],
                "created_at": message["created_at"],
                "in_reply_to": message.get("in_reply_to"),
                "target": message.get("target"),
            }
        )
        flatten_threads(node.get("replies") or message.get("replies") or [], issue_key, into)


def counts_only(coverage):
    """The item counts of a coverage answer and nothing else: its identity names a model service
    host, and this repository is public."""
    return {
        "total_items": coverage["total"]["items"],
        "kinds": {kind: row["items"] for kind, row in coverage["kinds"].items()},
    }


if __name__ == "__main__":
    main()

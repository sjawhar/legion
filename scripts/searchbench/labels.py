#!/usr/bin/env python3
"""Label the measured query set by what each searcher did next. Read-only over the session store.

Input is the pinned query set (dispatch://LEGION-386/artifact/query-set-json version 2). Each agent
search there names its session file and tool-call id; this reads those files again, because the query
set records only whether a returned issue was read, not which issue was opened when it was not
returned, and not what the session had already seen.
For every agent search that reached the server, from the session's tool calls in file order:
  hits         the issue keys its result listed, in the order listed (one per result line, the query set's
               miner's rule)
  hit_rows     each result row whose owner is an issue, parsed strictly (envoy-client's searchResultLine):
               its key, the kind of row production matched (issue, document, comment, ask or message),
               for a document whether it was the issue's primary document (its href opens the Spec tab)
               or another one, and the artifact name the row carried
  window       its next WINDOW tool calls, as the query set's next-step rule reads them
  seen_before  every issue key in the session's header, user messages, tool-call arguments and tool
               results before the assistant turn that sent the search
An open is a call in the window that names an issue key: a Dispatch read or write (a dispatch_* call
or a read of a dispatch:// path), or, for an issue the search returned, any read-shaped call (a local
read or a bash command naming it), as the query set's read_hit rule counts it.

Direct labels of a search S, each an issue key with the rule that produced it:
  returned   an open of a key S returned
  elsewhere  a Dispatch open of a key that no search from S's turn up to the open returned and that the
             session had not seen before S. It belongs to the latest search turn before the open; when
             that turn holds several searches on different topics it is ambiguous and labels none.
Chain labels: S's reformulation is the first later search in its window that shares a content word
with it (the query set's relation: same terms, added terms, dropped terms or rephrased). Following
reformulations to the last one, that search's direct labels label S too, as `chain` when S did not
return the key and `chain-returned` when it did.
A dashboard search is labelled by the issue the browser opened next within 60 s (`dashboard-opened`);
the logs hold no responses, so whether that issue was a result is unknown.

Usage: labels.py --query-set QS.json --out DATA/labels.jsonl
"""

import argparse
import collections
import json
import os
import re
import sys
import urllib.parse

KEY = re.compile(r"\b((?:AGENTC|LEGION|OPS|LEGSMOKE)-\d+)\b")
KEY_B = re.compile(rb"\b((?:AGENTC|LEGION|OPS|LEGSMOKE)-\d+)\b")
XD = re.compile(r"^xd://([a-z_]+)$")
CALL_ID = re.compile(rb'"toolCallId":"([^"]+)"')
WINDOW = 8
DISPATCH_READ = {"dispatch_read", "dispatch_doc_read"}
DISPATCH_ACT = {"dispatch_comment", "dispatch_message", "dispatch_issue_update", "dispatch_ask", "dispatch_claim",
                "dispatch_doc_edit", "dispatch_suggest", "dispatch_artifact", "dispatch_follow", "dispatch_request_approval"}


def describe(call: dict) -> tuple[str, str | None, dict]:
    """(tool, device, args): a write to xd://<device> is a call of that device with the written JSON."""
    name = call.get("name") or ""
    args = call.get("arguments") or {}
    if isinstance(args, str):
        try:
            args = json.loads(args)
        except ValueError:
            args = {"_raw": args}
    device = None
    if name == "write" and isinstance(args, dict):
        m = XD.match(str(args.get("path", "")))
        if m:
            device = m.group(1)
            c = args.get("content")
            try:
                args = json.loads(c) if isinstance(c, str) else (c or {})
            except ValueError:
                args = {"_raw": c}
    elif name.startswith("mcp__") or name.startswith("dispatch_"):
        device = name.rsplit("__", 1)[-1]
    return name, device, args if isinstance(args, dict) else {"_raw": args}


def is_search(name: str, device: str | None) -> bool:
    return device == "dispatch_search" or name.endswith("dispatch_search")


def content_words(q: str) -> set[str]:
    """The query set's own comparison (analyze.py content_words)."""
    return {w for w in re.findall(r"[a-z0-9_]+", q.lower()) if len(w) > 2}


def relation(a: str, b: str) -> str:
    """analyze.py next_action's relation of a later search b to a search a."""
    x, y = content_words(a), content_words(b)
    if y == x:
        return "same terms"
    if x and x < y:
        return "added terms"
    if y and y < x:
        return "dropped terms"
    if x & y:
        return "rephrased"
    return "different topic"


def hit_keys(text: str) -> list[str]:
    """Issue keys of a search result, one per result line in order (the query set's miner's rule)."""
    out = []
    for line in text.splitlines():
        m = KEY.match(line.strip()) or re.match(r"^\s*(?:\d+[.)]\s*)?((?:AGENTC|LEGION|OPS|LEGSMOKE)-\d+)", line)
        if m and m.group(1) not in out:
            out.append(m.group(1))
    return out


# A result is `KEY [status] title - <kind>[ <artifact name>]: snippet -> href` (envoy-client's
# searchResultLine, unchanged for issue-owned rows since search shipped). A snippet can hold newlines, so a
# row runs until the next row starts, and its href is the text after its last ` -> `.
ROW = re.compile(r"^((?:AGENTC|LEGION|OPS|LEGSMOKE)-\d+) \[[^\]]*\] .*? - (issue|document|comment|ask|message)\b")
DOC_ROW = re.compile(r"^dispatch://\S+ \[document\] ")


def href_kind(key: str, href: str) -> str | None:
    """The row kind api/search.go's searchHref encoded in an issue-owned href, at every version since search
    shipped: `primary document` (the issue's /spec route with ?q=), `other document` (its /artifacts/<slug>
    route with ?q=), comment (/comments/<id>, or ?comment= on a document), ask (/asks/<id>, ?ask= or a #b-
    block fragment), message (/log, or /messages/<id>) or issue (the issue's own route). A title can hold
    ` - comment`, so the label's kind word is only the fallback."""
    u = urllib.parse.urlsplit(href)
    query, path, base = urllib.parse.parse_qs(u.query), u.path.rstrip("/"), f"/issues/{key}"
    if "comment" in query or re.search(r"/comments/[^/]+$", path):
        return "comment"
    if "ask" in query or u.fragment.startswith("b-") or re.search(r"/asks/[^/]+$", path):
        return "ask"
    if path.endswith(base + "/log") or re.search(r"/messages/[^/]+$", path):
        return "message"
    if path.endswith(base + "/spec"):
        return "primary document"
    if re.search(re.escape(base) + r"/artifacts/[^/]+$", path):
        return "other document"
    if path.endswith(base):
        return "issue"
    return None


def hit_rows(text: str) -> list[dict]:
    """Every issue-owned result row in order: its key and the kind of row production matched, read from its
    href (href_kind), else from the label's kind word."""
    rows: list[list] = []
    for line in text.splitlines():
        m = ROW.match(line)
        if m:
            rows.append([m.group(1), m.group(2), line])
        elif DOC_ROW.match(line):
            rows.append([None, None, line])  # a project document's row: owned by no issue
        elif rows:
            rows[-1][2] += "\n" + line
    out = []
    for key, label_kind, body in rows:
        if key is None:
            continue
        kind = href_kind(key, body.rsplit(" -> ", 1)[1].strip()) if " -> " in body else None
        out.append({"key": key, "kind": kind or label_kind, "from": "href" if kind else "label"})
    return out


def open_kind(name: str, device: str | None, args: dict) -> str | None:
    """'dispatch' for a Dispatch read or write, 'local' for another read-shaped call, else None."""
    if device in DISPATCH_READ or device in DISPATCH_ACT:
        return "dispatch"
    if name == "read" and str(args.get("path", "")).startswith("dispatch://"):
        return "dispatch"
    if name in ("read", "bash") and device is None:
        return "local"
    return None


def parse_session(path: str, wanted: set[str]):
    """The session's tool calls in file order, each with its turn (assistant entry), the keys seen
    before that turn, and, for wanted searches, the result text."""
    calls, results = [], {}
    seen: set[str] = set()
    header = None
    with open(path, "rb") as f:
        for line in f:
            if header is None and b'"type":"session"' in line[:200]:
                try:
                    header = json.loads(line)
                except ValueError:
                    header = {}
                seen.update(KEY.findall(json.dumps({k: header.get(k) for k in ("title", "cwd")})))
                continue
            if b'"toolCall"' in line and b'"assistant"' in line:
                try:
                    rec = json.loads(line)
                except ValueError:
                    continue
                msg = rec.get("message") or {}
                if msg.get("role") == "assistant":
                    before = frozenset(seen)
                    turn = rec.get("id")
                    for c in msg.get("content") or []:
                        if c.get("type") != "toolCall":
                            continue
                        name, device, args = describe(c)
                        flat = json.dumps(args)
                        calls.append({"id": c.get("id"), "turn": turn, "name": name, "device": device, "args": args,
                                      "keys": list(dict.fromkeys(KEY.findall(flat))), "seen_before": before})
                    seen.update(k.decode() for k in KEY_B.findall(line))
                    continue
            if b'"toolResult"' in line:
                m = CALL_ID.search(line)
                if m and m.group(1).decode() in wanted:
                    rec = json.loads(line)
                    msg = rec.get("message") or {}
                    text = "\n".join(x.get("text", "") for x in (msg.get("content") or []) if isinstance(x, dict))
                    results[msg.get("toolCallId")] = {"is_error": msg.get("isError"), "text": text}
            # Every other line (user messages, tool results, custom entries) adds what it names to what
            # the session has seen.
            seen.update(k.decode() for k in KEY_B.findall(line))
    return header or {}, calls, results


def label_session(path: str, fleet: dict[str, dict]) -> dict[str, dict]:
    wanted = {q["provenance"]["tool_call_id"] for q in fleet.values()}
    _, calls, results = parse_session(path, wanted)
    idx = {c["id"]: i for i, c in enumerate(calls)}
    searches = [i for i, c in enumerate(calls) if is_search(c["name"], c["device"])]

    def text_of(i):
        c = calls[i]
        q = c["args"].get("query")
        return q if isinstance(q, str) else ""

    def hits_of(i) -> list[str] | None:
        """The keys a search listed, or None when the transcript does not show its result (no result
        recorded, an error, or a result compacted out of the transcript)."""
        r = results.get(calls[i]["id"])
        if not r or r["is_error"] or r["text"].startswith("[shaken"):
            return None
        return hit_keys(r["text"])

    out = {}
    for cid, q in fleet.items():
        i = idx.get(cid)
        if i is None:
            out[cid] = {"error": "tool call not found in session file"}
            continue
        c = calls[i]
        hits = hits_of(i)
        window = list(range(i + 1, min(len(calls), i + 1 + WINDOW)))
        reform = next((j for j in window if is_search(calls[j]["name"], calls[j]["device"])
                       and relation(text_of(i), text_of(j)) != "different topic"), None)
        base = {"hits": hits, "reformulation": calls[reform]["id"] if reform is not None else None,
                "filed_issue": any(calls[j]["device"] == "dispatch_issue" for j in window),
                "hit_rows": hit_rows(results[cid]["text"]) if hits is not None else None}
        if hits is None:
            out[cid] = {**base, "direct": [], "hits_unknown": True}
            continue
        direct, same_turn = [], 0
        turn_start = min(k for k in range(len(calls)) if calls[k]["turn"] == c["turn"])
        for j in window:
            d = calls[j]
            kind = open_kind(d["name"], d["device"], d["args"])
            if kind is None or not d["keys"]:
                continue
            if d["turn"] == c["turn"]:
                # Sent in the same turn as the search, so before its results were seen: not an outcome.
                same_turn += 1
                continue
            prior = [s for s in searches if turn_start <= s < j]  # S's turn up to this call
            latest_turn = calls[prior[-1]]["turn"]
            latest = [s for s in prior if calls[s]["turn"] == latest_turn]
            prior_hits = [hits_of(s) for s in prior]
            for k in d["keys"]:
                if k in hits:
                    direct.append({"key": k, "rule": "returned", "open": kind, "call": d["id"],
                                   "tool": d["device"] or d["name"], "seen_before": k in c["seen_before"]})
                    continue
                if kind != "dispatch" or k in c["seen_before"] or i not in latest:
                    continue
                if any(h is None for h in prior_hits) or any(k in h for h in prior_hits):
                    continue  # another search returned it, or might have
                # The open belongs to the latest search turn before it; several topics there is ambiguous.
                same_topic = all(relation(text_of(latest[0]), text_of(s)) != "different topic" for s in latest)
                rule = "elsewhere" if len(latest) == 1 or same_topic else "ambiguous"
                direct.append({"key": k, "rule": rule, "open": kind, "call": d["id"], "tool": d["device"] or d["name"],
                               "seen_before": False, "turn_searches": len(latest)})
        out[cid] = {**base, "direct": direct, "same_turn_opens": same_turn}
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--query-set", required=True)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    os.umask(0o077)
    derive(json.load(open(args.query_set)), args.out)


def derive(qs: dict, out_path: str) -> None:
    fleet = [q for q in qs["queries"] if q["source"] == "fleet-transcript"]
    by_file = collections.defaultdict(dict)
    for q in fleet:
        by_file[os.path.expanduser(q["provenance"]["session_file"])][q["provenance"]["tool_call_id"]] = q
    raw: dict[str, dict] = {}
    for n, (path, group) in enumerate(sorted(by_file.items()), 1):
        raw.update(label_session(path, group))
        if n % 100 == 0:
            print(f"{n}/{len(by_file)} session files", file=sys.stderr, flush=True)

    # Chains: follow each search's reformulation to the last one; its direct labels label the chain.
    by_call = {q["provenance"]["tool_call_id"]: q for q in fleet}

    def final_of(cid: str) -> list[str]:
        path, cur = [], cid
        while cur is not None and cur not in path and cur in raw:
            path.append(cur)
            cur = raw[cur].get("reformulation")
        return path

    records = []
    for q in fleet:
        cid = q["provenance"]["tool_call_id"]
        r = raw.get(cid, {"error": "session not parsed"})
        rec = {"id": q["id"], "source": q["source"], "at": q["at"], "context": q["context"],
               "reached_server": q["reached_server"], **r}
        labels = [x for x in r.get("direct", []) if x["rule"] in ("returned", "elsewhere")]
        chain = final_of(cid) if "error" not in r and r.get("hits") is not None else []
        if len(chain) > 1:
            last = raw[chain[-1]]
            rec["chain"] = [by_call[c]["id"] for c in chain]
            for x in last.get("direct", []):
                if x["rule"] not in ("returned", "elsewhere"):
                    continue
                rule = "chain-returned" if x["key"] in r["hits"] else "chain"
                labels.append({"key": x["key"], "rule": rule, "via": by_call[chain[-1]]["id"], "final_rule": x["rule"]})
        # One label per key: a direct label wins over a chain label for the same key.
        seen, uniq = set(), []
        for x in labels:
            if x["key"] not in seen:
                seen.add(x["key"])
                uniq.append(x)
        rec["labels"] = uniq
        records.append(rec)
    for q in qs["queries"]:
        if q["source"] != "dashboard-sjawhar":
            continue
        opened = (q.get("next") or {}).get("opened_other_issue_within_60s")
        records.append({"id": q["id"], "source": q["source"], "at": q["at"], "context": "work", "reached_server": True,
                        "labels": [{"key": opened, "rule": "dashboard-opened"}] if opened else []})
    with open(out_path, "w") as f:
        for rec in records:
            f.write(json.dumps(rec) + "\n")
    print(f"{len(records)} records written to {out_path}", file=sys.stderr)


if __name__ == "__main__":
    main()

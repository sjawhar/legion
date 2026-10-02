# Asks after they open: who answers, editing, resolving, replying, and following

`skill://dispatch` sends you here once you are asking: to find who answers an ask, hand a human a
to-do, edit or retract an ask, answer a clarification, say whose turn it is, or follow or leave its
thread. Whether to ask at all, and how to write the question, is "Before you ask" in
`skill://dispatch`.

## Who answers an ask

An ask goes to the issue's assignee: their Inbox opens on **Mine**, which lists asks on the issues they hold plus an Unassigned band; an ask on an unassigned issue waits in that band for someone to take it. Find out who Dispatch takes you for with:
```ts
dispatch_whoami({})
```
It returns `details` `{ session, owner, service }`: `owner` is the lowercase email of the person whose personal token you run under, or `null` under the shared token or a verified service token, in which case `service` is that token's subject (`system:serviceaccount:<namespace>:<name>`, `null` otherwise) and every write you make is rendered `(as <namespace>/<name>)` — the namespace is kept because every namespace has a `default` service account, and a subject that is not a Kubernetes one is shown whole. An issue you create without `assignee` goes to your owner; with no owner it inherits its parent's assignee, or stays unassigned without a parent. If an issue you are asking on is unassigned and the answer matters, assign it to your owner (`PATCH /api/v1/issues/{key}` with `{"assignee": "<email>"}`; any authenticated caller may reassign, and an email nobody has signed in with is refused with `ASSIGNEE_NOT_ALLOWED`) or name in the question who should answer it. Never reassign an issue a human holds to get an answer faster: that is the human's call.

## Handing a human a to-do, editing, resolving, and replying

A to-do handed to a human is an ordinary question: phrase the to-do as the question and give it
the options that name its outcomes, in the human's words - there is no fixed vocabulary and the
server treats no label specially. If an outcome needs a reason, say so in that option's
description, and the human's free-text answer carries it:
```ts
dispatch_ask({ issue: "DSP-42",
  question: "Run the production deploy for #19125?",
  options: [{ label: "Deployed" }, { label: "Blocked", description: "Say what is missing." }] })
```

Correct or refine an open ask in place instead of opening a second question:
```ts
dispatch_edit_ask({
  ask,
  question?,
  options?: { label, description? }[],
  multiple?,
  urgency?,
})
```
At least one field besides `ask` is required. Use this only while the same decision remains open: it keeps the prior text in the event
log and invalidates any answer draft against the prior `edited_at` revision, so the human sees the new wording and explicitly reconfirms.
An answered or resolved ask cannot be edited. If the decision is moot or superseded, retract the old ask and open a new one.

An ask that lives as an `ask` block in a document keeps its question and options in the block, and `dispatch_edit_ask` writes the
block along with the row, so the edit stands and the document reads the same. It changes only the fields you name: pass `urgency`
alone and the question's own wording, formatting, links and comment anchors are untouched. Pass `question` or `options` and that part
is rewritten, so anchors inside the text you replaced move as they would for any document edit. Either way it is a document edit: it
writes a new version, which on a spec awaiting approval closes the design gate until the new version is approved. Editing the block
with `dispatch_doc_edit` works too and is the way to change anything else about it, including adding formatting to a question.
Re-sending a field unchanged rewrites nothing, so retrying the whole ask is safe.
Text the block cannot carry back unchanged is refused outright, naming the field and writing nothing - an option label containing
`": "`, the separator between a label and its description, is one example of text that cannot survive the round trip. Blank lines separate paragraphs;
a single newline is kept as a line break.

An ask stays open until a human answers, unless its question no longer needs that answer. Retract a moot or superseded question, or
self-resolve one after finding the answer:
```ts
dispatch_resolve_ask({
  ask,
  kind: "retracted",
  reason: "A newer ask supersedes this question.",
})
```
Use `retracted` when the question is obsolete and `resolved` when you found the answer. Include the reason because the question remains
in its Conversation card and reply thread; a reason beginning `removed from the document in version` is refused, because that is how a
retraction the document's own settlement wrote is recognised. Resolving a block ask records it in the block too, so it stays resolved
however the document moves afterwards. `dispatch_doc_edit` refuses deleting the block of an open ask; a block removed another way, by a
whole-document `dispatch_artifact` replace or by a person in the browser, closes its ask, and putting the block back reopens it. Resolution is not an answer: it never records a human decision, and an answered ask cannot be
resolved. A human may reply to an open or answered ask; so may you, e.g. after finding the answer — use `reply_to_ask` on
`dispatch_comment` (mutually exclusive with `reply_to`).
A review comment you opened has its own closer, `dispatch_resolve_comment` — see
[Comments and suggestions](skill://dispatch/references/documents.md).

A human answers or asks back from the same field; a question-shaped free-text answer is offered as a clarification first. A human
reply while your ask is still open (the delivered `comment.created` carries `ask_state: open`) is a request for clarification, not
an answer: the human did not understand the question or needs more before choosing. The ask now waits on you in their Inbox. Answer
in the same thread with `dispatch_comment({ reply_to_ask })`, or reword the question itself with `dispatch_edit_ask` when the wording
was the problem; either puts the ask back in front of them. Do not open a second ask.

Every reply to an open ask says whose turn it is next, and the Inbox files the ask by that, not by who spoke last. Your plain reply
(`turn` omitted, or `turn: "human"`) hands the turn to the human: the ask returns to their `Waiting on you`. When you are not done
yet — "dispatched two auditors, back with results", "checking the release branch, back shortly", any working-on-it note — reply with
`turn: "agent"`: the note lands in the thread, the ask stays under `Waiting on agents`, and the human is not told to act. Use
`turn: "agent"` for every progress note and `turn: "human"` (the default) only when you need them. A human's reply always hands the
turn to you. The result names the state (`ask now waiting on agent` / `human`), the delivered `comment.created` carries it as
`ask_waiting_on`, and every ask read carries it as `waiting_on`.

## Following

An ask has followers: every session that wrote to it — the session that opened it and every session that replied with
`dispatch_comment({ reply_to_ask })` — plus any session a human adds from the ask card. The ask's answer, edits, resolution, and
every reply on it reach each follower's own agent topic directly, whether or not the writer was a human and whatever the issue's
route. The tool result says so (`You follow this ask: its answer and replies reach you directly.`) and carries `details.follows.ask`;
the host tells you once per ask. Leave a thread you no longer need, or rejoin one, with:

```ts
dispatch_follow({ ask, action: "follow" | "unfollow" })
```

`ask` is the full ask id or a `dispatch://KEY/ask/<id>` reference; `dispatch_follow`, `dispatch_edit_ask`, and
`dispatch_resolve_ask` also take an 8+ hex prefix that is unique among your own open asks, and refuse anything shorter by naming
those asks. A human may also remove you from the ask card; either way you are
told with an `ask.follower_removed` notice, and a human adding you arrives as `ask.follower_added`.

No write subscribes you to an issue or document. Following covers your own asks and the threads you joined; everything else on the
owner — other sessions' asks, comments, messages, status changes — reaches you only if you subscribe to the owner topic yourself.
Every write result names that line: `envoy_subscribe notifications.dispatch.issue.<KEY>.>` for an issue,
`envoy_subscribe notifications.dispatch.document.<PROJECT>.<SLUG>.>` for a project document. The owner topic carries every Dispatch
event; `notify` only controls agent wake and routed delivery. A human may unsubscribe you from the issue or document header; you
are told with a `subscription.removed` notice when that happens.


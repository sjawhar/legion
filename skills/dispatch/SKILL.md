---
name: dispatch
description: "Use before posting a message, a status update, or a periodic status update; before asking a question that references another message, artifact, or eval; and when asking Sami a question, updating the spec, commenting on a document, attaching an artifact, or calling a dispatch_* tool."
---

# Dispatch

Dispatch is your issue's or project document's living spec, asks, comments, and artifacts — a high-signal record for the humans who
decide, never a log of your work. The transcript is your scratch pad; progress and status stay there. Anything meant for a human
goes through a `dispatch_*` tool.

The server enforces high signal: an ask question is at most 800 characters with at most eight options; comment and message bodies are at
most 2,000 characters; an artifact is at most 25 MiB. It refuses over-limit input with the number to trim (`question is 50 characters over
the 800-character limit (850/800)`); it never truncates it. A tool call with several problems is refused once, every problem listed
(`<tool> was not called: N problems`), so one corrected call lands. GitHub threads and markers no
longer exist.

## Design changes are brainstormed here

Sami, 2026-09-17, verbatim: "Make sure your agents know that they should be doing brainstorming with me
through dispatch for major design changes." For a major design change the design conversation itself
happens in Dispatch: write the spec document early, while it is still a draft with real alternatives, and
put each open question in it as an `ask` block beside the options and trade-offs it depends on
([Writing a spec](#writing-a-spec), [Typed blocks](#typed-blocks)). He answers in place and the document
grows into the record. A finished spec dropped after a chat-only design, or a set of one-line issue asks
pointing at a document, is not brainstorming with him.
Sami, 2026-09-17, verbatim: "Can you please stop doing this thing where you have these one-off,
shorthand, compressed decision asks that are completely disconnected from any discussion of the
design or the trade-offs? This is just very obviously not the most effective way to have a design
communication." A question lives beside the options and trade-offs it depends on, in the spec or
discussion it came from — never as a compressed standalone ask.

## Writing for the human

Sami, 2026-09-12, on what Legion had been producing: "It's completely incomprehensible. It's just
compressed jargon nonsense. I have no idea what the fuck it's saying." Every spec, ask, comment,
message, and PR body is read by a person who has not read the code, does not share this session's
vocabulary, and is often on a phone. Write for that person.

- Plain English, full sentences, one idea per sentence. Never repo shorthand or nouns you coined:
  not "fix 8c", "READY-target", "PR B", "spec@v3", "the pair", "the packet" — say what the thing is.
- Expand every identifier the first time it appears: an issue key gets its title, a PR number its
  title, a file what it is for, a session id who it is. Link a URL rather than pasting a bare id.
- A question lives beside the options and trade-offs it depends on, in the spec or discussion it
  came from — never a compressed standalone ask (his words are quoted under [Design changes are
  brainstormed here](#design-changes-are-brainstormed-here)). Give the reader the options, what
  each costs, and your recommendation with its reason; do not prescribe yourself a form.
- Before posting, test it: could Sami, reading only this text on his phone, know what he is being
  told or asked? If not, rewrite it. Length is not the problem; density is.
- When an ask or message communicates a judgment, lead with that judgment in one sentence and put the mechanism underneath it. Do not make the reader ask a second time whether the result is a win. This shapes communication only when a judgment exists; it does not pre-decide an open question or remove its genuine options. Inferred from the AGENTC-186 12-hour-cap incident (platform PO, 2026-09-17).
- When a Dispatch message states a root cause, include the reproducing command or test in that same message. Without it, label the diagnosis a hypothesis; a diagnosis still in progress may say so plainly. This boundary applies to causal claims, not to reporting that an investigation has started. Inferred from the astro lane's 2026-09-16 retro (platform PO, 2026-09-17).

## Agent authentication

Use a personal Dispatch token: a human mints it in Dispatch **Settings → Agent tokens** and supplies
it to the agent through `dispatch.token` in `~/.config/opencode/envoy.json` or `DISPATCH_TOKEN`.
The server records the minting human as the owner of that session's writes. `DISPATCH_AGENT_TOKEN`
is the shared devbox fallback; do not configure it for an individual agent.

The deployed Dispatch server's browser origin is configured separately with
`DISPATCH_SERVER_URL` in the deployment `compose/.env`. Do not change an
agent's `envoy.json` to set the GitHub OAuth callback origin: the value must
be the exact URL humans type in their browser, and the GitHub App callback is
`<DISPATCH_SERVER_URL>/auth/callback`.

### Finding a route

The tools cover the everyday surface. For anything else, ask the server: `GET /api/v1` (no
credential) returns every route as `{method, path, auth, description}` sorted by path — `auth`
is `public`, `any` (a human or a bearer), `human` (a bearer gets `403 HUMAN_ONLY`), or `bearer`.
A path Dispatch does not serve under `/api` or `/v1` answers
`404 {"code":"NOT_FOUND","error":"no route for GET /v1/issues","hint":"GET /api/v1 lists every
route"}`; when you see that, you typed the path wrong — read the index rather than guessing. Every
`/api/v1` error body carries a `code`; branch on the code, never on the text.

## Writing a spec

A spec has two readers: the human who decides reads the top; the implementer who builds reads the
rest. Use these headings in this order.

| Section | Required content | Form |
| --- | --- | --- |
| **Summary** | The problem, what changes for whom, and how we will know it worked — in plain words. | Three sentences at most. |
| **New since we talked** | Every design point the human did not settle in conversation, marked `inferred:` with the reasoning. Empty is fine. | One plain sentence per point. |
| **Acceptance** | Each outcome names what a user will observe and the check that proves it (browser scenario, API call, or command). An outcome without a check is not acceptance. | Numbered lines. |
| **Requirements** | What must hold, and where each came from: a quoted human sentence, or `inferred:` plus the reasoning. Readers treat inferred requirements as hypotheses. | `requirement \| where it comes from` table, or prose if the reader follows it more easily. |
| **Design** | The files, components, routes, and data flow that change. | Prose or tables; a diagram only for real structure. |
| **Errors** | The behaviour for every error condition. Never a silent fallback. | `condition \| behaviour` table. |
| **Testing** | Which proof exercises each acceptance line. | One line per acceptance item. |
| **Rejected** | Each alternative considered and why it was rejected, so it is not proposed again. | One alternative per line. |

### Rules

- The spec is the issue's one primary document. Extend it in place — a new version that keeps the
  human's own text — never a second "spec" artifact beside it.
- No hedging ("might", "could consider"). No TBD, TODO, or placeholders: an open item is either
  an ask block or a question for the platform PO whose ruling becomes a Requirement (see
  [Before you ask](#before-you-ask) under Asking).
- Keep each section to one screen; work that exceeds one screen per section is two specs.
- Update the spec as decisions land: the spec is the record, comments are the discussion.
- Before sending it: no sections conflict, every requirement has exactly one reading, and the
  Summary and every ask block pass the phone test above.

## Decision blocks

A decision a human must make is an `:::ask` block where the decision arises in the spec: inside
the section whose content it is about, never gathered into a list at the top or bottom. Sami,
LEGION-204 comment, 2026-09-20 14:47Z, verbatim: "Adding a bunch of decision blocks at the top is
terrible!! Decisions should be in context in the spec".

The block is what reaches the human's Inbox. A question phrased as prose in the spec reaches
nobody. A spec with no ask blocks is fine only when the issue genuinely needs no human decision.
When `dispatch_issue` or `dispatch_artifact` answers `No decision blocks in this spec …`, read it
as a question, not an error: either no decision is needed and you say nothing, or you forgot to
make the decision a block and must fix the spec.

**Wrong:** a **Decisions needed** list at the top of the spec with three bullets.
**Right:** put each decision in the design section it belongs to as an `:::ask{#slug}` block, with
2–4 options, a recommendation, and surrounding prose that explains the trade-off.

See [Typed blocks](#typed-blocks) for the syntax and [Before you ask](#before-you-ask) under
[Asking](#asking) to decide whether the question is a real decision at all.

## Your owner

Every session works on an issue or project document. Legion pre-fills `issue` from `LEGION_ISSUE`: use a native issue key such as
`LEGION-3`, an external `owner/repo#n` reference, or a bare positive number (resolved against the cwd repository). Otherwise pass
exactly one owner to every owner-scoped tool: `issue` for an issue, or `project` and `artifact` for an unlinked project document (see
[References](#references) for the resulting ref shape). On first use, an external issue reference creates its native issue in the
project configured for that repository in Dispatch Settings, then falls back to `DISPATCH_DEFAULT_PROJECT`.

Issue reads include `rank`, the server-owned ordering key used by project boards; reorder through `PATCH /api/v1/issues/{key}` with neighboring issue keys. They also include nullable coarse priority (`P0` highest through `P3` lowest) and `assignee`: the lowercase GitHub login of the human who answers the issue's asks, or `null` when nobody holds it. `dispatch_read` of an issue prints it as `Assignee: <login>` or `Assignee: unassigned`.

### Who answers an ask

An ask goes to the issue's assignee: their Inbox opens on **Mine**, which lists asks on the issues they hold plus an Unassigned band; an ask on an unassigned issue waits in that band for someone to take it. Find out who Dispatch takes you for with:
```ts
dispatch_whoami({})
```
It returns `details` `{ session, owner }`: `owner` is the lowercase login of the human whose personal token you run under, or `null` under the shared token. An issue you create without `assignee` goes to your owner; under the shared token it inherits its parent's assignee, or stays unassigned without a parent. If an issue you are asking on is unassigned and the answer matters, assign it to your owner (`PATCH /api/v1/issues/{key}` with `{"assignee": "<login>"}`; any authenticated caller may reassign, and an unlisted login is refused with `ASSIGNEE_NOT_ALLOWED`) or name in the question who should answer it. Never reassign an issue a human holds to get an answer faster: that is the human's call.

Architects create newly tracked child work with:
```ts
dispatch_issue({ project, title, parent?, external?, spec?, force?, labels?: string[], priority?: 0 | 1 | 2 | 3, assignee?: string })
```
`labels` are optional initial labels: Dispatch trims them, preserves their case, and removes case-insensitive duplicates. Set `priority` on creation only when the human's intent makes the bucket clear; otherwise priority remains the human's decision. Set `assignee` (a GitHub login on the sign-in allowlist) only when the human said who owns the work; otherwise the default above applies, so a child inherits its parent's assignee. It returns
`details` `{ issue }`; creating an issue does not subscribe you to it (see [Following](#following)). Use `dispatch_issue` only to create an issue; never use it to park a question. When `spec` is supplied,
follow [Writing a spec](#writing-a-spec).

## Issue status is yours to move

The issue's status is how a human sees delivery without asking a session. Outside Legion (where
the daemon writes it), the session doing the work moves it, the way a person moves a card:
`in_progress` when implementation starts, `testing` when the change is being proven on a
production-like surface, `needs_review` when its pull request is open and waiting on the merge
queue, `done` when the change has been driven in production (a merge is not `done`). Move child
issues you own as well as the root. An issue left at `triage` while work is underway is a defect:
Sami, 2026-09-15, on the roadmap he could not read — "I'm not even sure what their development
status is." Waiting for the deploy lane is not a status and is never announced. Priority stays the
human's: set it on creation only when their intent is clear, and change it only on their word.

```ts
// PATCH /api/v1/issues/{key} — status, title, labels, external_links (merged by URL), route, parent
dispatch_issue_update({ issue: "AGENTC-175", status: "testing" })
dispatch_issue_update({ issue: "AGENTC-175", external_links: ["https://github.com/owner/repo/pull/7"] })
dispatch_issue_update({ issue: "AGENTC-175", parent: "AGENTC-170" }) // same-project key; "" clears the parent
```

Link the pull request that delivers the issue in `external_links` when you open it; the issue page
renders its state and checks from that link. The call is authenticated with the same bearer as every
other `dispatch_*` tool: a Legion pane reads it from the `DISPATCH_TOKEN_FILE` path the daemon sets on
the pane; an OMP session outside Legion reads `dispatch.token` from `~/.config/opencode/envoy.json`.

A write to an issue still in `triage` answers once with `… is still in triage …`; move the status
when work has started.

## Search first

Before you create an issue or start a design document, search:
```ts
dispatch_search({ query, project?, limit? })
```
It returns every issue, document, comment, ask, and message that contains the words. Issue-owned hit lines
start with the issue key; standalone project-document hit lines start with
`dispatch://PROJECT/artifact/<slug>`, followed by the absolute link. Cite the hit you build on
(`dispatch://KEY` or the document reference), or state "no prior issue" in the spec. Websearch syntax applies:
`"merge queue"`, `-daemon`, `OR`.

`dispatch_issue` refuses a title that near-duplicates an issue in the same project and returns the candidates (`POSSIBLE_DUPLICATE`).
Read them; reference the existing issue, or repeat the call with `force: true` when it is genuinely new work.

## Reading a project's backlog

To see the shape of a project rather than find a phrase, list its issues:
```ts
dispatch_issues({ project, status?, parent?, label?, updated_since?, limit? })
```
Each row carries the issue key, title, status, priority, parent, labels, its open-ask count, and
when it last changed — a roadmap or backlog pass without opening every issue. Filter with `status`
(a lifecycle status), `parent` (one issue's children), `label`, or `updated_since` (an RFC3339
timestamp, for "what moved this week"). `limit` caps the rows at 50 by default and 250 at most.

This is not search: it matches no text. Use `dispatch_search` for a keyword or phrase, and
`dispatch_issues` when you want every issue in a project and its current state.

## Asking

### Before you ask

Sami, 2026-09-16, verbatim, rejecting two asks the same night: "All of these \"decisions\" are
completely disconnected from any discussion of design or trade-offs. This is not a very useful way
of having this discussion" (on a report-table shape), and "What's a fenced PutObject or phantom
eval_id? What's an R4 model header? What exactly is the question or uncertainty here?" (on a
production import). Every `dispatch_ask` passes three gates first:

1. **Does it need his authority, taste, or risk appetite?** The same bar as a spec's Decisions
   needed ([Writing a spec](#writing-a-spec)). Schema shapes, table layouts, field names, migration
   internals, and contracts between lanes do not: they go to the platform PO over Envoy, who rules.
2. **Is there genuine uncertainty?** If not, it is a plan you execute. The one legitimate ask
   without uncertainty is permission for an action only a human can authorise — a production
   write, an external send, a console action — and then the question is that action in one
   sentence, with options that name its outcomes (below).
3. **Can someone who has not read the code answer it on a phone?** What he can see today, what
   changes for a reader, two options with what each costs, your recommendation. No slice or
   decision numbers, no coined nouns, no internal identifiers he has never used, no jargon you
   would have to define. This is the phone test in [Writing for the human](#writing-for-the-human).
   If you cannot write it that way, you do not understand it well enough to ask.

The platform PO audits open asks. One that fails a gate — or that points at another message in
prose instead of carrying its content (below) — is retracted, with the PO's answer as the record.

Open a decision with:
```ts
dispatch_ask({
  issue?,
  project?,
  artifact?,
  ref?,
  question,
  options?: { label, description? }[],
  multiple?,
  urgency?,
  anchor?: { artifact, quote, occurrence? },
})
```
It returns `details` `{ issue, ask, follows: { ask } }` for an issue or `{ project, artifact, document, ask, follows: { ask } }` for a project document: you follow the ask you opened (see [Following](#following)).

References belong in the question text; `ref` is sugar that appends its `dispatch://` value to the question as a rendered link.

An ask is read on a phone by someone who has not read the code. Open with one or two plain
sentences: what needs deciding and why it matters now. Each option is a button with a label and
one sentence saying what happens if it is chosen; never enumerate choices in prose. Put the
recommendation and its reason last, in `question`. Never put file paths, line numbers, sequence
numbers, document versions, or role tokens in the question; if the human needs that detail, anchor
the ask to the document passage instead. Apply the phone test from "Writing for the human" before
posting. Anchor a document question with `anchor: { artifact, quote, occurrence? }`; `occurrence`
is zero-based and selects a repeated quote. A quote anchor is pinned to its lowest complete
containing block while retaining its quote as display text, so rewording the passage keeps it
attached; a quote spanning top-level blocks, and existing anchors without a block, stay readable
against their original document version if their quote disappears.

An ask must be answerable from its own text and its anchor alone. Anchor a question about a document
passage with `anchor`. Follow up on an ask or comment with `dispatch_comment`; cite anything else
with a `dispatch://` reference (see [References](#references)). Never write "see above", "the
message above", or "as attached".

**Pointing at another message is a defect, not a shortcut.** Sami, 2026-09-17, verbatim, on an
ask that read "the settings listed in my comment just above" after a long procedure had been posted
as a comment: "you just dump information into messages and then add a new ask that references a
previous message in prose with no link or no context whatsoever and uses compressed shorthand
jargon." The ask view does not show the issue's comments, so that ask was unanswerable; "Cloud
Identity licence check / 2SV override / 1-day grace" was shorthand he had never used. The rules
that follow from it:

- An ask that names another message in prose — "my comment above", "the procedure I posted",
  "see the earlier message" — is retracted by the PO as failing the gates. Put the content IN the
  ask. If it does not fit the 800-character budget, the step is too big: split the step, never
  point elsewhere. The only pointers an ask may carry are a `dispatch://` reference or a document
  `anchor`, and they cite — the ask still says in one line what the reader will find there and can
  be answered without following them.
- Expand every term the reader has not used first. A product name, an internal setting, an
  acronym, a value you coined this session — write what it is in the ask, in his words.
- A runbook the human must execute is one ask per step, each self-contained: what to do, where,
  what result proves it, and options that name the step's outcomes. Each later step opens only
  after the previous is answered and states that step's verified result in one line ("Step 1
  done: the licence shows Cloud Identity Free on the admin console.") — never a pointer to the
  earlier ask.

**A decision about an uploaded artifact links it.** If the human must read an artifact to answer,
the question carries `dispatch://KEY/artifact/<slug>` (or `ref`), never just its filename. Text
they must read to decide belongs in the spec in the first place — see [Artifacts](#artifacts).

Attach an issue to the architecture components it changes, before decomposing it — children inherit the parent's attachment unless they choose their own, so attaching the root once classifies the whole tree:

```
dispatch_issue({ project: "CORE", title: "...", components: { mode: "explicit", ids: ["dispatch-server", "web"] } })
dispatch_issue_update({ issue: "CORE-12", components: { mode: "explicit", ids: ["web"] } })   // this issue's own set, replacing what it inherited
dispatch_issue_update({ issue: "CORE-13", components: { mode: "none", reason: "hiring, not code" } })
dispatch_issue_update({ issue: "CORE-14", components: { mode: "inherit" } })                  // back to the parent chain's attachment
```

Component ids are the file names under the repository's `.dispatch/architecture/` (`web.md` → `web`); an id the model lacks, or an `external` component, is refused with `COMPONENTS_INPUT`. A closed issue can be classified without reopening. Two rules: declare and attach before decomposing work, and change code and its architecture description (`.dispatch/architecture/<id>.md`) in the same review — the tree at `GET /api/v1/projects/{key}/architecture` counts every issue whose effective set names a component or anything it contains.

Import a project's architecture model from its configured source repository now (a human configures the source in Settings):
```ts
dispatch_architecture_sync({ project: "CORE" })
```
It returns the imported commit, or the recorded error when the model was rejected — the previous model stays up. Without a configured source it answers 404 `SOURCE_NOT_FOUND`.

Before saying you are waiting for human input, call `dispatch_open_asks`. With no arguments it lists this session's active asks across open issues and project documents, including whether the human or agent owes the next reply. With `dispatch_open_asks({ project })` it lists every open ask in that project — on its issues and on its documents, whoever authored them — which is how you audit what a whole project is waiting on rather than just your own asks.

**Unsettled product shape needs a decision before implementation.** When a page, navigation entry, table key, customer-scoping rule, or persisted sidecar would set product shape that Sami has not already settled, send a one-line ask before the first implementation commit. A platform-PO schema or contract ruling does not settle product shape. This does not turn a user-specified decision or routine implementation into an approval request; it is inferred from AGENTC-186's 2026-09-16 retro (platform PO, 2026-09-17).

**Anything that needs the human is an ask, or it does not exist.** An approval, a credential,
a setting only they can change, a review click, a conflict between two of their own rules - if
your work waits on it, open a `dispatch_ask` the moment you know, the action as the question.
Never write it into a spec, a comment reply, a message, or a
pull-request body: nothing in those paths reaches the human's Inbox, and a human who is not
reading your document does not know they are the blocker. Before asking, try to remove the
step: a value already on the machine, a permission you already hold, an API that replaces the
click. One ask per item, `urgency: "high"` when work is stopped on it; while it is open, keep
working on everything that is not.

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
in its Conversation card and reply thread. Resolution is not an answer: it never records a human decision, and an answered ask cannot be
resolved. A human may reply to an open or answered ask; so may you, e.g. after finding the answer — use `reply_to_ask` on
`dispatch_comment` (mutually exclusive with `reply_to`).
A review comment you opened has its own closer, `dispatch_resolve_comment` — see
[Comments and suggestions](#comments-and-suggestions).

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

## Close what you opened

An ask you opened is yours until it is answered or you resolve it. When the answer arrives some
other way — Sami said it live, a later comment settled it, or the question became moot because the
design moved — resolve it yourself with `dispatch_resolve_ask` in the same turn you learn that.
Never leave it for the human to clear.

Sami, AGENTC-27 `rules-derivation-2026-09-17.md` row R04, verbatim: "I think this was answered
live. If not, please reask." The same pattern left him closing asks as `Dismissed`, `Settled`, and
`Resolved I think`: noise the human had to clear.

Every later write on the issue answers `You still have an open ask on …` and names it. Treat that
as the checklist: if it is still needed, leave it; if it was answered elsewhere, resolve it with
the resolving fact as the reason. Before posting a new ask, inspect your open ones. If the new ask
supersedes one, retract the old one with `dispatch_resolve_ask` and kind `retracted` in the same
turn.

See [Following](#following) for why you receive what happens to asks you open and
[What comes back](#what-comes-back) for finding them again with `dispatch_open_asks`.

## Approval of a spec

Approval is a property of a document, not a question you phrase: a human approves a specific version, the way a pull-request
review approves a commit, and any later edit makes that approval stale. It is the exception, not a step for every issue - reach
for it when a spec departs from what the human already settled, proposes children, or when the project has armed a design gate.

```
dispatch_request_approval({ issue?, project?, artifact? })
```

Opens (or returns the open) approval ask for the document at its latest version - options `Approve` and `Request changes`, in
the human's Inbox like any ask. The answer reaches you as `artifact.approved` or `artifact.changes_requested` with the pinned
`version`; `changes_requested` carries the reason, which is your next piece of work. `dispatch_read` and `dispatch_doc_read` show
the document's approval state; `stale` means it was approved and then edited - request again for the new version. Never write
"Approve" options into an ordinary `dispatch_ask`, and never approve anything yourself: only humans review.

## The Spec

The spec holds requirements, design, acceptance, decisions, and rejected alternatives, structured per [Writing a spec](#writing-a-spec).
It changes only when a decision or requirement changes, and every version that records one is named with `summary`. Never write
progress, status, timestamps, an "Update HH:MMZ" section, a PR list, or handoff notes into the spec. Progress is not a
Dispatch object at all: it lives in your transcript and your pull request (see [Messages](#messages)).

Read the current document before changing it:

```ts
dispatch_doc_read({ issue?, project?, artifact?, version?, ref? })
```
It returns live or versioned markdown with open marks. A live read ends with a document token; `issue` with an
omitted `artifact` reads the issue specification; a project needs `artifact`; and a
`dispatch://PROJECT/artifact/<document-ref>` ref supplies both, where `document-ref` is the id, slug, or filename.

```ts
dispatch_doc_edit({ issue?, project?, artifact, ops, precondition?, summary? })
```
It returns issue or project-document owner details plus `applied` and optional `version`. `ops` is an array of this
exact `EditOp` shape:

```ts
type EditOp = {
  op: "replace" | "delete" | "insert" | "retype" | "move" | "delete_row" | "delete_column";
  find?: string;
  with?: string;
  occurrence?: number;
  markdown?: string;
  after?: string;
  before?: string;
  block?: string;
  index?: number;
  type?: string;
  attributes?: Record<string, unknown>;
};
```

Target `replace`, `delete`, and quote insert anchors by a block's text as rendered: write inline
code without backticks, bold without asterisks, and link text without link syntax. A table-cell
anchor is its cell text. Quote code-block contents without their Markdown fences. A quote must stay
within one textblock; split changes that span separate blocks into separate operations.

`replace` requires `find` and `with`; `delete` requires `find` or `block`; `insert` requires `markdown` and exactly one of `after` or
`before`; `move` requires `block` and exactly one of `after` or `before`; and `delete_row` / `delete_column` each require a table
`block` plus a zero-based `index`. An insert or move anchor is a quote, `"start"`, `"end"`, `"heading:Title"`, or `"block:<id>"`.
Ordinary inserts create a sibling block before or after the quote, heading, or block's enclosing document block, and a move lands the
block at that same boundary; `"start"` and `"end"` select the document edges. At a table-cell quote, a body-row fragment (no header or
delimiter rows) extends that table before or after the matched row instead; short rows are padded, wider rows are rejected, and deleting
a cell's quoted text removes only that text. `delete_row` / `delete_column` instead mutate their named table in place, keeping the
table's block id. A row index includes the header: row `0` is the header and its deletion promotes the first body row. The last body
row and any row's last column cannot be deleted. An index is required. A missing, non-integer, negative, or out-of-range index is
`INVALID_OP` on `index`, naming the supplied value and the table's actual dimensions before making any change. Markdown parsing
canonicalizes short ragged rows by padding missing cells, so column deletion preserves every non-selected cell in the canonical table.
`GET /api/v1/artifacts/<artifact UUID>/blocks` reports a table's own references plus its descendant cell anchors. A row or column
deletion that would remove an open ask or unresolved comment anchor is `INVALID_OP` on `index`, naming the axis and anchor ids;
answered asks and resolved comments are history and do not block it. A `find` or quote anchor tolerates inline Markdown
(`**bold**`, `` `code` ``) and a leading `# ` selects a heading by its text; a miss names the three nearest blocks so the next quote
lands. `replace` is inline: `with` is the new text of the matched span inside its block, so a leading list or heading
marker (`4. Design`, `# Title`) stays literal text and never turns the block into a list or heading; `with` that forms more than one
paragraph is rejected (`INVALID_OP` on `with`) — delete the block and insert new blocks instead. Use zero-based `occurrence` for a
repeated target; re-read a missing or ambiguous target before retrying. Pass `summary` to name the version when recording a decision.

A `delete` whose `find` is a block's entire text removes the block itself — the bullet, paragraph, or heading, not just its words — and
a list emptied of every item disappears with it; a partial match keeps the block with its remaining text. Deleting the text of a bullet
that holds a nested list hoists that list's items into the bullet's place (as an outliner does); a bullet with any other content
(paragraphs, code, tables) is refused with `INVALID_OP` naming `delete {block:"<item id>"}`, which removes the item with its content.
`delete` with `block` removes any block by id (paragraph, heading, list, list item, table, or typed block; deleting an open `ask` block
retracts its ask, while an answered one keeps its answer as the record), and `move` with `block` relocates one, keeping its id and
attributes — a moved `ask` keeps its ask and answer. Block ids are the `#id` a typed block renders
(`:::ask{#5467e5ce-…}`) and, for every block including untyped ones, the `id` rows from
`GET /api/v1/artifacts/<artifact UUID>/blocks` (or `/api/v1/issues/{key}/artifacts/{slug}/blocks`), each with its `type` and byte range
in canonical markdown; the UUID route does not accept a slug. A later operation in the same atomic batch that names a block removed by
an earlier `delete {block}` fails as `INVALID_OP` naming the earlier operation and the parent block that cascaded the removal. A move
whose anchor lies inside the moved block, or a delete that would leave a typed block without the body its content rule requires, is
`INVALID_OP` naming the field and the rule.

`GET /api/v1/artifacts/<artifact UUID>/blocks` includes a full-state `token` on every block, including
inline marks. To reject a stale edit, pass `precondition` with exactly one of
`{ document: "<token from dispatch_doc_read>" }` or
`{ blocks: [{ id: "<block id>", token: "<block token>" }] }`. The server resolves the whole batch before
mutation: a block guard must cover every content block it changes, or Dispatch returns
`400 INVALID_PRECONDITION` without applying anything. Use a document token for insert and move because they
depend on document order. A block token lets other sections change concurrently; a new anchored ask or comment
changes the relevant token. A stale guard returns `409 PRECONDITION_FAILED` with each mismatch and current
token; Dispatch applies no part of that batch. It is the hashline `#TAG` property applied to stable block ids,
not line numbers: canonical Markdown lines shift under concurrent edits and rendering changes, while block ids
survive moves and retyping.

`retype` turns the paragraph or typed block with `block` into the named typed `type` in place. It keeps the
block id, keeps a typed block's body, and uses `attributes` for client-owned typed attributes. Use it when
an existing paragraph is the question that should become a decision.

## Typed blocks

The server declares typed document blocks at `GET /api/v1/schema/blocks`. Write one only with the
container-directive form `:::name{#block-id key="value"}` on its own line, ordinary block children,
and a closing `:::` at the same nesting. An unclosed typed block at document level is rejected. For
a new typed block, omit `#block-id`; Dispatch mints it. When editing an existing typed block, retain
its id and every rendered attribute.

Use only the type names, content rule, attributes, and enum values returned by the schema. Values are
quoted: `:::callout{kind="warning" title="Risk"}`. Do not write Pandoc-style `::: {.callout}`, leaf
`::name` directives, or text `:name` directives; those strings are literal when quoted inside a code
block. Do not set attributes the schema marks `server: true`; the server ignores them and reasserts
its authoritative value at settlement.

Questions about a document must be `ask` blocks, never an `Open questions` prose section. An ask
body is one or more question paragraphs followed by an optional bullet list of options, where each
item is `Label: description`. For example:

```md
:::ask{urgency="high" multiple="false"}
Should we ship the migration?

- Ship: Release the verified change.
- Hold: Wait for another review.
:::
```

When a human answers a decision written as an ask block, the answer lives on that ask. Use
`dispatch_resolve_ask` when the decision is resolved without a human response, or preserve the
human's answer; never rewrite the question into its answer or blank its options. An edit that leaves
an ask block without a question or with a blank option is rejected with `INVALID_ASK_BLOCK`.

## Comments and suggestions

Add feedback with:

```ts
dispatch_comment({ issue?, project?, artifact?, ref?, quote?, occurrence?, body, reply_to?, reply_to_ask?, turn? })
```

It returns issue or project-document owner details plus `comment`; a `reply_to_ask` reply also returns `ask` and `follows: { ask }`,
because replying to an ask makes you one of its followers (see [Following](#following)).
`ref` names the owner (an issue or project-document reference) in place of `issue`/`project`.
`quote` requires `artifact`; its anchor is pinned to the containing block while retaining the quote
for display. Omit both for a floating issue comment. A reply (`reply_to`/`reply_to_ask`) takes no
`quote`; it belongs to its parent's anchor. Reply to any comment in a thread; the server keeps
threads flat. A reply to a resolved thread reopens it. Use `reply_to_ask` to reply directly under a
question asked with `dispatch_ask`; `turn` (only with `reply_to_ask`) says who holds the turn after
the reply — `agent` for a progress note that keeps the ask waiting on you, `human` (the default) when
the human needs to act; see [Asking](#asking). Comments are edited only by their author from the
dashboard. A delivered `comment.created` event carries the comment `id`; reply to it with
`dispatch_comment({ reply_to: <id> })`.

Resolve your own review comment once you have addressed it:

```ts
dispatch_resolve_comment({ comment })
```

`comment` is the comment id, or a `dispatch://KEY/comment/<id>` or `dispatch://PROJECT/artifact/<slug>/comment/<id>` reference (an
8+ character id prefix is resolved against the owner's comments). It returns the owner details plus `comment`, and takes no reason —
say what you did in a `reply_to` first if the thread needs it. The server lets any session or human resolve any open comment, so
resolve only threads you opened or were asked to close; reopening a resolved thread is human-only (from the dashboard), though your
reply to it reopens it. Asks are closed with `dispatch_resolve_ask` instead.

An exact replacement for document text is a suggestion (`dispatch_suggest`), never a comment; a
comment is for a question or a note the human answers in words. A human accepts a suggestion with
one click and cannot accept a comment, so propose the replacement instead of describing it:

```ts
dispatch_suggest({ issue?, project?, artifact, ref?, quote, replace_with, body?, occurrence? })
```

It returns issue or project-document owner details plus `comment`. A human accepts or rejects a suggestion.
Errors: `TARGET_AMBIGUOUS` (add `occurrence`), `TARGET_NOT_FOUND` (re-read first), `INVALID_ANCHOR`/`ANCHOR_MISSING`/`ANCHOR_ORPHANED`
(bad, unwritten, or stale quote), `INVALID_MARKDOWN`/`DOC_SCHEMA` (malformed content), `CAP_EXCEEDED`, `ISSUE_CLOSED`.

## Artifacts

Attach an image, diagram, or local file with:

```ts
dispatch_artifact({ issue?, project?, name, path, summary? })
```

Or, when the text is already in the call, post a Markdown document directly:

```ts
dispatch_artifact({ issue?, project?, name: "load-test-results.md", content: "# Load test\n..." })
```

Exactly one of `issue` and `project` is required. A project upload creates an unlinked project document; it must not include `artifact`.
Exactly one of `path` and `content` is required. It returns issue or project-document owner details plus `artifact` and `version`.
Uploading the same `name` creates its next version — so uploading `spec.md` **replaces the issue's own specification**
with your text. Never do that: the spec is edited in place with `dispatch_doc_edit` (see [The Spec](#the-spec)). Address an existing
artifact by the slug shown in the upload result or by its filename, and a project document by its artifact id, slug, or filename; the
slug also arrives on `artifact.created` events.

**Where a deliverable goes.** Text the human must read to decide — a draft message, a proposal,
a summary — goes in the spec as a section: the spec is the one document they open. A separate
artifact is for a real file: something sent as-is, a long report, a binary, a screenshot.

When you do upload one, the spec links it as `dispatch://KEY/artifact/<slug>` (the `slug` from the
upload result; it renders as a link) at the place the reader needs it, and the ask that needs the
decision carries the same reference. A heading or a sentence naming the filename is not a
reference.

Documents are CommonMark. A bare `<https://example.com|text>` is a CommonMark autolink and is normalised: the angle brackets are
dropped and the URL keeps `|text`. A backslash-escaped `\<https://example.com|text>` displays as `<https://example.com|text>` in the
document but comes back re-escaped (`\<`) from `dispatch_doc_read`. A Slack mrkdwn draft, or any other payload that is not Markdown,
still belongs inside a fenced code block, where it survives verbatim both ways.

## Structure over stream

Dispatch is a structured workspace, never a message stream (Sami, 2026-09-13, verbatim: "strange
to me that agents keep trying to use dispatch as a giant stream of messages instead of
high-signal, structured conversation"). The structure IS the product:

- **One issue per piece of work.** A new deliverable — an email to send, a document to review, a
  decision with its own lifecycle — gets its own issue with the content as the issue's document
  (spec artifact), not a message pile on an existing issue. If you are about to post a message
  carrying a draft, a spec, or anything over a couple of paragraphs, stop: that is an issue with a
  document, or an artifact on the issue it belongs to.
- **Content lives in documents; decisions live in asks; messages only announce.** A draft the
  human must read goes in a document artifact the dashboard renders with versions and margins; the
  ask that needs their word references it (`ref`, or the `dispatch://` link inline) instead of
  restating it. A message never carries a body a human has to scroll.
- **Never split one deliverable across a message + an ask that points at it.** Ask the question
  with the document reference in the question text; the reader lands on the content in one click.

The tool result answers your third consecutive message on an issue with no human reply with
`You've sent N messages …`. That is the ledger pattern being named; stop and either wait or ask
once.

## Messages

Dispatch is a high-signal record for humans, not a log of what you are doing. A message is a reply to a human's message, or a
change a human must know about now: a deliverable landed, a blocker only they can clear. Nothing else — no progress updates, no
"starting X", no "still working", no restating the spec, no status on a timer. Your transcript is where work is narrated; the
pull request is where it is summarised. One message that a human reads beats ten that train them to skip you.

```ts
dispatch_message({ issue, body })
```

It returns `details` `{ issue, message }`. `body` is capped at 2,000 characters. A message is not a decision
(`dispatch_ask`) or document feedback (`dispatch_comment`), and it does not wake anyone unless the issue is routed.

### Targeted agent messages

A human — or any bearer caller over HTTP, such as a test rig — can target the issue message at a
live Envoy session or role as **BTW**, **Aside**, or **Steer**. The incoming Dispatch frame names
the issue and includes a `reply_with` hint (`{ tool, args }`, ready to issue on any host); reply on the same open issue with the existing
tool, never a new targeted send:

```ts
dispatch_message({
  issue: "CORE-1",
  in_reply_to: "<targeted-message-id>",
  body: "The requested answer.",
})
```

`in_reply_to` correlates the answer under the asker's message in its Conversation card. A BTW
delivery can post its answer automatically; use this call when the frame asks the primary agent to
reply. A human may reply to your message in turn — the follow-up arrives as a targeted frame whose
`in_reply_to` names your message and whose `reply_body` quotes it; answer it the same way,
`dispatch_message({ issue, in_reply_to: "<their reply id>", body })`, so the exchange reads as one
thread. `dispatch_message` itself never carries `target` or `delivery`: agent-to-agent traffic goes
through Envoy or the hub. A bearer that targets over HTTP names its own session in `actor`
(`{kind: "session", id}`), and the card shows that session as the author. `GET /api/v1/agents`
(any authenticated caller) lists live sessions with their capabilities (`aside`, `btw`, `steer`);
target only a session that advertises the mode you want. Sending to a session with no issue
(`POST /api/v1/agents/{session_id}/messages`) stays human-only.

## Following

An ask has followers: every session that wrote to it — the session that opened it and every session that replied with
`dispatch_comment({ reply_to_ask })` — plus any session a human adds from the ask card. The ask's answer, edits, resolution, and
every reply on it reach each follower's own agent topic directly, whether or not the writer was a human and whatever the issue's
route. The tool result says so (`You follow this ask: its answer and replies reach you directly.`) and carries `details.follows.ask`;
the host tells you once per ask. Leave a thread you no longer need, or rejoin one, with:

```ts
dispatch_follow({ ask, action: "follow" | "unfollow" })
```

`ask` is the full ask id or a `dispatch://KEY/ask/<id>` reference. A human may also remove you from the ask card; either way you are
told with an `ask.follower_removed` notice, and a human adding you arrives as `ask.follower_added`.

No write subscribes you to an issue or document. Following covers your own asks and the threads you joined; everything else on the
owner — other sessions' asks, comments, messages, status changes — reaches you only if you subscribe to the owner topic yourself.
Every write result names that line: `envoy_subscribe notifications.dispatch.issue.<KEY>.>` for an issue,
`envoy_subscribe notifications.dispatch.document.<PROJECT>.<SLUG>.>` for a project document. The owner topic carries every Dispatch
event; `notify` only controls agent wake and routed delivery. A human may unsubscribe you from the issue or document header; you
are told with a `subscription.removed` notice when that happens.

## What comes back

After a restart, catch up with:

```ts
dispatch_read({ issue?, project?, artifact?, ref? })
```

With an issue ref, it returns the issue summary, open asks, references, and recent events with `details` `{ issue }`. With a project
document owner or ref, it returns a document summary with `details` `{ project, document }`. With an ask ref, it returns that ask's
question, options, state, answer, and its reply thread. With a comment ref, it returns that comment and its quoted reply chain. With a
message ref, it returns that message and its reply chain. Reads do not subscribe; use `dispatch_doc_read` for document contents.

Every read ends with two sections from the reference graph. `Referenced by:` lists what points at the node — every document, ask,
comment, or message that cites it, plus its structure: child issues, attached documents, anchored and owned asks and comments, replies,
followers — and `Links:` lists what it cites. Each row is `- <edge kind> <node kind> dispatch://… (<excerpt> · <when>)`; for a
document source the excerpt is the block containing the mention. Cross-project, always: a message on another project's issue that
cites an ask shows up under that ask. So "what led to this decision" is one `dispatch_read` on the ask, and "who relies on this
document" one read on the document. Cite with `dispatch://` references (below) whenever you name a node in a body — a bare id or
title is invisible to the graph.

## References

**Every reference is a link, never an unlinked mention.** If you name a thing that has an address, link it: another issue, ask, comment, spec, or message (the `dispatch://` forms below), an artifact (`dispatch://KEY/artifact/<slug>`), an eval (its viewer URL), a Slack message (its permalink), a Drive file (its share link). Bare phrases like "see this eval", "his 09-04 run", "the comment above", or "per the spec" with no link are banned: they make the reader hunt for what you already had in hand, and nothing can be traversed from them. Linking every reference is what makes a body both consumable and navigable. If a thing genuinely has no linkable address, say so; otherwise the link is not optional.

Use these in document, ask, comment, and message bodies. In the dashboard, a reference renders
as an inline link whose text is the target's title (an issue's title, an ask's question, a
comment's first line, a document's name) once it resolves; a body that is only a bare reference
still gets an unfurl card instead. Every `ref` argument below (and `issue`/`project`) accepts
either form — an issue key or a project key is never ambiguous, since a project key never
contains a dash:

```text
dispatch://KEY
dispatch://KEY/spec
dispatch://KEY/artifact/<slug>[@vN]
dispatch://KEY/ask/<id>
dispatch://KEY/comment/<id>
dispatch://KEY/message/<id>
dispatch://PROJECT/artifact/<document-ref>[@vN]
dispatch://PROJECT/artifact/<document-ref>/ask/<id>
dispatch://PROJECT/artifact/<document-ref>/comment/<id>
```

A bare UUID or `KEY#seq` is not a reference; the `dispatch://` form is what Dispatch links and records. `dispatch_read` also
accepts the dashboard URL of an issue, spec, artifact, ask, comment, or project document on the configured server (it maps to the
`dispatch://` form above), and an ask or comment id may be a unique prefix of at least 8 hex characters; a message id is always the
full uuid. A non-uuid id on `GET /asks/{id}`, `/comments/{id}`, or `/issues/{key}/messages/{id}` is a 400 `ASK_ID_INPUT` /
`COMMENT_ID_INPUT` / `MESSAGE_ID_INPUT`, never a 500.

## Before / after

Before — a wall of text hides the decision and makes the choices unclickable:

```text
We need to settle the release gate because the deploy branch has the migration and the
dashboard changes, I checked the staging result and it is fine except the release notes are
not reviewed, so should we ship today, wait for docs, or cut the dashboard from this release?
I think waiting is safest but the customer demo is tomorrow and the list above is probably stale.
```

After — anchor the decision and make each option a button:

```ts
dispatch_ask({
  issue: "LEGION-815",
  question:
    "Choose the release gate. Recommendation: ship after release-note review, since the tested deployment is otherwise ready.",
  options: [
    { label: "Review notes, then ship", description: "Keeps the release intact and reviewed." },
    { label: "Ship now", description: "Meets the demo deadline; release notes follow later." },
  ],
  urgency: "high",
  anchor: { artifact: "spec", quote: "Release requires reviewed operator instructions before deployment." },
})
```

Before — a progress note that nobody needs, posted where humans look for decisions:

```ts
dispatch_message({ issue: "LEGION-815", body: "Merged the release PR, moving to docs next." })
```

After — nothing. The merge is visible on the pull request; the docs work shows up as its own deliverable. Post a message only when
a human must act or a deliverable is theirs to use:

```ts
dispatch_message({
  issue: "LEGION-815",
  body: "Release 1.4 is live on the devbox (dispatch://LEGION-815/artifact/release-notes). Nothing needed from you.",
})
```

Before — a draft the human must read is uploaded as a separate file, the spec only names it, and
the ask does not point at it, so the reader has to go looking:

```ts
dispatch_artifact({ issue: "OPS-52", name: "cu-update-2026-09-15.md", content: "Hi team, ..." })
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft (artifact cu-update-2026-09-15.md)" },
]})
dispatch_ask({ issue: "OPS-52", question: "Send the customer update as drafted?", options: [...] })
```

After — the draft is a section of the spec, and the ask anchors there. If it really must be a
file (something to send as-is), the spec and the ask both link the slug from the upload result:

```ts
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft\n\nHi team, ..." },
]})
dispatch_ask({
  issue: "OPS-52",
  question: "Send the customer update as drafted?",
  options: [...],
  anchor: { artifact: "spec", quote: "Hi team," },
})
// or, for a real file — the spec links it where the reader needs it, and so does the ask:
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft\n\nThe update to send as-is: dispatch://OPS-52/artifact/cu-update-2026-09-15-md" },
]})
dispatch_ask({
  issue: "OPS-52",
  question: "Send this customer update as-is? dispatch://OPS-52/artifact/cu-update-2026-09-15-md",
  options: [...],
})
```

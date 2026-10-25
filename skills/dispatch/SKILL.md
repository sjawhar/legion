---
name: dispatch
description: "Use when posting to Dispatch: a message, status update, comment, artifact, spec or document edit, claim or status change. Use when asking a human a question, also one that cites another message, artifact or eval. Use when brainstorming or asking about a design change, or when running any dispatch command."
---

# Dispatch

Dispatch holds an issue's or project document's spec, asks, comments and artifacts. It is a high-signal record for the humans who decide, never a log of your work. Narrate progress in your transcript, not here.

- Write to Dispatch only with the `dispatch` command in your shell. `dispatch --help` lists the commands. `dispatch <command> --help` gives the flags, the limits and an example.
- Put long text on stdin: `--<field>-file -` with a quoted here-document (`<<'EOF'` … `EOF`). Give object and list fields as JSON (`--ops-json`).
- Exit code 0 means it wrote or read, 1 means Dispatch refused, 2 means a usage error.
- Dispatch refuses over-limit input with the number to trim. It never truncates. It lists every problem of a call in one refusal, so fix them all and call once again.

## Where the detail lives

Open a reference when you reach its step, not before. On a host with no `skill://` scheme (Claude Code, OpenCode), the path after `skill://dispatch/` is relative to this skill's directory.

| Before you | Read |
| --- | --- |
| run `dispatch doc-edit` | [Editing a document](skill://dispatch/references/document-edits.md) |
| write a typed block, comment, suggest, upload an artifact, request approval, or retry `DOC_SERVICE_UNAVAILABLE` | [Documents](skill://dispatch/references/documents.md) |
| choose, claim or move an issue, set priority, reorder a board, or audit a backlog | [Working an issue](skill://dispatch/references/issues.md) |
| route, edit, retract, resolve or reply to an open ask, or follow its thread | [Asks after they open](skill://dispatch/references/asks.md) |
| catch up after a restart, trace what cites a node, or write a `dispatch://` reference | [Reading back](skill://dispatch/references/reading.md) |
| answer a BTW, Aside or Steer frame, or an Agents-page message | [Targeted and direct messages](skill://dispatch/references/messages.md) |
| want a worked example | [Before and after](skill://dispatch/references/examples.md) |
| set up a token, or call a route no command covers | [Authentication and the HTTP API](skill://dispatch/references/api.md) |

## Design changes are brainstormed here

A design change that needs the human's choices is brainstormed in its issue's spec, not in chat or a repository design document. Its plan is the issue's `plan.md` document. Read `skill://dispatch-brainstorming` before a spec's first version, before each next turn, and before you write a plan.

## Writing for the human

Every spec, ask, comment, message and PR body goes to a person who has not read the code. They do not share your vocabulary, and often read on a phone.

- Write plain English in full sentences, one idea each. Never use repo shorthand or nouns you coined ("fix 8c", "PR B", "spec@v3", "the pair"). Say what the thing is.
- Name a real product, tool operation (`dispatch request-approval`), event (`artifact.approved`), route, setting or status exactly. Say in a clause how it works. Never use a vague verb such as "notice", "accept", "tell" or "pick up" ([example](skill://dispatch/references/examples.md#naming-the-mechanism)).
- Expand each identifier on first use. Give an issue key or PR number its title, a file its purpose, a session id who it is. Link a URL, not a bare id.
- Describe a change first by what its reader stands to lose. Say who can do what today, what they cannot do after it, what still works, and what you cannot tell. Then name each mechanism. Write that first sentence with a human subject. A shorter engineering sentence is not a reader's sentence.
- When the text carries a judgment, lead with it in one sentence and put the mechanism under it. Then the judgment comes first, and the rule above shapes the sentence under it. This never pre-decides an open question or drops its real options.
- When you state a root cause or a blocker, include the command, error output or record that shows it, with its time. Without it, call the diagnosis a hypothesis. A diagnosis in progress can say so.
- Phone test before you post: can the human, reading only this text on a phone, know what they are told or asked? If not, rewrite. Density is the problem, not length.

A question lives in the spec or discussion it came from, as [Decision blocks](#decision-blocks) says, never as a compressed standalone ask. It carries the problem the reader recognises, why it matters now, what constrains the answer, and your recommendation with its reason. It asks how to solve the problem or which outcome is wanted. It never lists the choices: the options carry the different approaches, each a label and a description of what it costs.

## Writing a spec

A spec is the design conversation with the human, written down. It starts as the problem and its evidence: what goes wrong, for whom, and the counts or cases that show it. It grows in place. It is the issue's one primary document: extend it with a new version that keeps the human's text, never a second "spec" artifact.

- **Each open question is a decision block** ([Decision blocks](#decision-blocks)).
- **An answered decision block is the record.** It shows the chosen option, the person's words, who and when. Never restate it.
- **Mark an inference as one**, with its reasoning. A security concern, a restriction or a cost nobody measured is an inference, not a settled constraint. A point carried from another document keeps its provenance. `skill://dispatch-brainstorming` keeps an inference out of a live brainstorm's spec until the human agrees to it.
- **A decision from a comment, conversation or waiver has no rendered record.** Write it into the text in the person's words, quoted and dated. Name them from `dispatch whoami` or the conversation. If neither names them, write "the person". Never use a name you were not given.
- **Sections follow the topic.** No heading is required or fixed.
- **Rewrite a changed point. Do not append.** The decision block stays as the record. Versions keep the history.
- **No placeholders**: no TBD, TODO or hedging. An open item is a decision block or a two-lane contract you settle over Envoy. Or it is a technical decision your lane makes where the work happens, outside the spec.
- **No progress**: no status, timestamps, "Update HH:MMZ" sections, PR lists or handoff notes.
- **Repeated measurements** (hourly counts, a fleet table) go in one artifact that you edit in place, linked once from the spec. Never post them as ask replies or as new spec rows.
- **No commentary**: never call it a draft, version or turn, preview a later version, describe a correction, or narrate the exchange.

Before a version goes out, read it as the human will. No two passages conflict, each point has one reading, and each decision block passes the phone test.

## Decision blocks

A decision a human must make is an `:::ask{#slug}` block at the end of the section that discusses it. Shape it as [Writing for the human](#writing-for-the-human) says. Split what constrains it into measured, known and unknown. Give 2–4 options and a recommendation, with the trade-off in the prose around it. It asks how to solve the problem, never whether to apply a change already chosen.

- Never gather decisions in a list, at the top, at the bottom, or in an "open questions" section. Never ask one as a standalone `dispatch ask` that points at the spec.
- Prose questions reach nobody. Only the block reaches the Inbox. A spec with no blocks is right only when no human decision is needed.
- Fix a pile with `move`, not a rewrite. `dispatch doc-edit --ops-json '[{"op":"move","block":"<block-uuid>","after":"<last sentence of its section>"}]'` keeps the ask, its answer and its followers. Move the lifted context the same way and delete the empty section. An ask block has two different ids ([Editing a document](skill://dispatch/references/document-edits.md)).
- Syntax and checks: [Typed blocks](skill://dispatch/references/documents.md#typed-blocks). Whether to ask at all: [Before you ask](#before-you-ask).

## Your owner

- Every owner-scoped command takes exactly one owner. That is `--issue` (a key such as `LEGION-3`, `owner/repo#n`, or a bare number for the cwd repository), or `--project` with `--artifact`. Legion pre-fills `--issue` from `LEGION_ISSUE`.
- An external reference addresses the Dispatch issue linked to it. If none is linked, create one: `dispatch issue --external owner/repo#n --project <project> --title '<title>'`.
- Create an issue for new work with `dispatch issue --project <KEY> --title '<title>'`. Set `--priority` ([issues.md](skill://dispatch/references/issues.md#priority-is-yours-to-set)). Set `--assignee` only when the human said who owns it. Set `--repo` only when it differs from what the issue inherits. Creating an issue does not subscribe you. Never create an issue to park a question.
- File work in a project that exists (`dispatch projects`). Work across projects goes in the one that owns most of it. Create a project only when the human asks. You cannot rename or delete a project.

## Search first

- Search as `skill://dispatch-first` says before you plan, file, ask, post, start a design document or start work. `dispatch search --help` has the syntax and paging. A paraphrase can match by meaning. Without an embedder, search says so and ranks by keyword. Never paste a passage: a query over 1,000 characters is refused. Cite the hit (`dispatch://KEY`) or say "no prior issue".
- `POSSIBLE_DUPLICATE` compares title words only. Read the candidates. Reference the existing issue, or use `--force` for new work. Then give the new title the difference, and open its spec with the distinction, citing the named issue.
- **Symptom versus cause.** Work goes on the issue whose title names the fix. The symptom's issue gets a pointer. Before you post a finding, search for the failing component's name. A symptom issue that gathers messages with no human reply is the sign.

## Claim the issue before you work it

- Before you implement, `dispatch claim --issue <KEY>` and `dispatch issue-update --issue <KEY> --status in_progress`. Release with `--release` when you finish, hand over or switch. Closing releases it.
- Claiming does not deliver the issue's comments to you. In the same turn, route the issue to your session, `dispatch issue-update --issue <KEY> --route session:<your session id>`, or subscribe to it with `envoy_subscribe notifications.dispatch.issue.<KEY>.>`. Never route an issue with the `legion` label: Legion's roles work it, not your session.
- Reading, commenting, asking or gating a PR claims nothing.
- `409 ISSUE_CLAIMED`: someone else holds it. Never work it in parallel. Refusals and takeovers: [issues.md](skill://dispatch/references/issues.md).

## Issue status is yours to move

Outside Legion, the session doing the work moves its issue and the children it owns. Use `in_progress` at implementation and `testing` while proven on a production-like surface. Use `needs_review` while its PR waits on the merge queue, and `done` with `--reason` once driven in production. Never write the status of an issue with the `legion` label or under one. The daemon writes those.

## Asking

A design decision a document records is a decision block in it, at every phase, approved or not. A block in an approved document makes the approval stale until its answer. A Legion phase worker sends the decision to its architect (`skill://legion-worker`). A to-do or permission only a human can give, or a decision with no document, is a `dispatch ask`. The gates apply to both.

In a Legion session, a role or any of its subagents that finds a problem outside its own issue never files an issue for it (the issue creation in [Your owner](#your-owner) included). It opens a `dispatch ask` as `skill://legion-worker/references/findings.md` says, on the issue that reference names. That overrides "Ask on your own issue" in gate 1, and gate 4's rule to ask nothing beside what the human asked for.

### Before you ask

1. **Does it need the human's authority, taste or risk appetite?** Your lane decides schema shapes, field names and migration internals. Decide them in the plan or code, not in the spec. Settle a two-lane contract over Envoy, with no ask. A halt condition passes the gate. That is an IAM change, deleting or exposing production data, or anything that reaches a customer. Ask on your own issue.
2. **Is there real uncertainty, and did you measure what you can?** With none, execute. The one ask without uncertainty is permission only a human can give: a production write, an external send, a console action. Start it with the problem and why, then what it changes and risks. Options name outcomes. Measure first: how many are affected, what reaches the path, the current state. Put the measurement and the population size in the ask. One fix cannot fix most of that population and another can? Drop the first. Never offer it cut down. An option not to act (Hold) stays. Report what the measurement could not establish, with its own control. "I found no evidence" and "there is none to find" differ. A control with the query's blind spot proves neither. The test for a new ask is not whether you asked on this issue before. It is whether it makes the human report again an answer they already gave.
3. **Can someone who has not read the code answer it on a phone?** Say who can do what today and what changes. Give two options with costs, and your recommendation. No slice or decision numbers, no coined nouns. If you cannot write it so, you do not understand it yet.
4. **Is it outside what the human already said they want?** If not, it is settled, with every choice inside it their words leave open. Build it and ask nothing about it or beside it until delivered, whatever the ask says about the build. After that, ask only about a choice their message raised and left open, quoting it and naming what you delivered. A cause you diagnosed or a neighbouring behaviour is not in their message: do not ask about it or build it. Fixing the cause of the thing they named is delivery. Do not turn a complaint about one control into a choice about another. Still ask at once about any blocker in [When you need a human](#when-you-need-a-human), including a conflict between their request and another of their rules.

Nobody else audits or retracts your asks. Passing the gates is your own check.

```bash
dispatch ask --issue <KEY> --question-file - --option 'Label: what it costs' --option 'Other: what it costs' <<'EOF'
<the problem, what constrains the answer (each answer of theirs you rely on, linked), your recommendation and why>
EOF
```

- You follow the ask you open. `--ref` appends a `dispatch://` link to the question.
- Never lead with a file path, line number, sequence number, version or role token. Evidence under it can cite one. Anchor it to a passage with `--anchor-json '{"artifact":"spec","quote":"…"}'` (optional zero-based `occurrence`).
- The ask must be answerable from its own text and anchor. The ask view does not show the issue's comments. Never write "see above", "my comment above" or "as attached". Put the content in the ask. If it does not fit in 800 characters, split the step. A `dispatch://` reference or an anchor may cite, with one line on what it holds.
- A runbook for the human is one self-contained ask per step: what, where, what result proves it, and outcome options. Open each step after the previous is answered, with its verified result in one line.
- If the human must read an artifact to decide, link `dispatch://KEY/artifact/<slug>`, never a filename. Text needed to decide belongs in the spec ([Artifacts](#artifacts)).

### When you need a human

- Before you say you wait on a human, run `dispatch open-asks`. Add `--project <KEY>` to see every open ask in a project.
- **Unsettled product shape needs a decision before implementation.** Product shape can come from a page, navigation entry, table key, customer-scoping rule or persisted sidecar. If the human has not settled it, write a decision block in the work's document before the first implementation commit. In a Legion tree the architect writes it. A lane's schema choice or a two-lane contract does not settle product shape. A behaviour the human asked for in words is settled, with each choice inside it. Routine work needs no approval.
- **Anything you are blocked on a human for is visible in Dispatch.** An agent waits on a human only through an open ask. Make each blocker a `dispatch ask` the moment you know. Blockers are a to-do, a permission, a credential or grant renewal, a human-only setting, a review click, or a conflict between two of their rules. Start with the problem and why, then constraints, options with costs, and your recommendation. For a human-only action, state what it changes and risks. Never make the action the question. A to-do only in a spec, comment, message or PR body reaches no Inbox.
- First try to make the step unnecessary: a value on the machine, a permission you hold, an API that replaces the click.
- One ask per item, `--urgency high` when work halts on it. Keep working on everything else.
- **Ask once.** Do not raise an open ask again in each message. The open ask is the request.
- After it opens: [Asks after they open](skill://dispatch/references/asks.md).

## Architecture components

- Attach every architectural issue to the components it changes. Attach the root before you decompose it: children inherit.
- `--components-json '{"mode":"explicit","ids":["web"]}'`, `'{"mode":"none","reason":"hiring, not code"}'` (only for non-architectural work, with a concrete reason), or `'{"mode":"inherit"}'`.
- Component ids: each non-external `components[].id` from `GET /api/v1/projects/{key}/architecture`, or the file names under `.dispatch/architecture/` in its source repository. Others are refused `COMPONENTS_INPUT`. A closed issue can be classified without reopening.
- Change code and `.dispatch/architecture/<id>.md` in the same review. A stale model: `dispatch architecture-sync` ([issues.md](skill://dispatch/references/issues.md#syncing-a-projects-architecture-model)).

## Close what you opened

- An ask you opened is yours until answered or resolved. When the answer arrives another way, or the design made it moot, `dispatch resolve-ask` in the same turn. Never leave it for the human.
- Each later write names your open asks (`You still have an open ask on …`). Keep the ones still needed. Resolve the rest with the resolving fact as the reason.
- Before a new ask, check your open ones. Retract one it supersedes with `--kind retracted` in the same turn.

## Approval of a spec

A human approves one version. A later version makes the approval stale, unless the human's own answer to a decision block is the whole change. Then Dispatch records the approval and emits `artifact.approved`: request nothing. If the answer changes text outside its block, rewrite that text, run the fresh reader, and request again. A new version also moves an open request, which then waits on you.

- Request only when all three hold: every block is answered or waived, the human agreed to every point, and this version is not approved. A Legion root spec under an armed design gate always goes to approval then (`skill://legion-architect`).
- First have a fresh read-only reader check the spec against the code and the human's words, and resolve every finding. A Legion architect uses `plan-gap-analyst`. Then request in the same pass: a finished design with nothing in the Inbox waits on nobody.
- If the human asks for approval while a block is open, name each open block and ask them to answer or waive it. For a waiver, `dispatch resolve-ask --kind resolved` with their words as `--reason`, write the decision into the text, then request.
- A request carries nothing new. An unagreed point first gets a decision block with your recommendation. A choice that is yours (gate 1), where the human will not want a say, leaves the spec. An inference you cannot defend in a block leaves the spec.
- `--summary` is one to three sentences on what they approve, with no commentary and no question. After an earlier approval, one sentence on what changed.
- Only humans approve. Never put "Approve" options in a `dispatch ask`. Details: [Approval requests](skill://dispatch/references/documents.md#approval-requests).

## The Spec

- Name each version that records a decision with `dispatch doc-edit --summary`.
- Read the current document with `dispatch doc-read` before you change it. `--blocks` lists block ids and tokens for edits. It also shows pictures: open a written picture file with your file-read tool.
- Edit in place as [Editing a document](skill://dispatch/references/document-edits.md) says.

## Typed blocks

Write an `:::ask` or a callout only as [Documents](skill://dispatch/references/documents.md#typed-blocks) says.

## Artifacts

- Text the human must read to decide (a draft, a proposal, a summary) is a section of the spec. An artifact is for a real file: sent as-is, a long report, a binary, a screenshot.
- Link an uploaded artifact as `dispatch://KEY/artifact/<slug>` where the reader needs it, and in the ask that needs it. A filename is not a reference. Uploading: [Documents](skill://dispatch/references/documents.md#artifacts).

## Structure over stream

- One issue per deliverable (an email to send, a document to review, a decision with its own life), with the content as its document. A message that carries a draft, a spec or more than a couple of paragraphs is an issue or an artifact instead.
- Content lives in documents, decisions in asks. Messages only announce. Never split one deliverable into a message and an ask that points at it: put the reference in the question.
- `You've sent N messages …` means three messages in a row with no human reply. Then wait, or ask once.

## Messages

A message is a reply to a human's message, or a change a human must know now, such as a deliverable that landed. Never progress, "starting X", "still working", spec restatements or timed status. The PR summarises the work.

```bash
dispatch message --issue <KEY> --body-file - <<'EOF'
<what landed, with its link>
EOF
```

- A message is not a decision or document feedback (`dispatch comment`). It wakes nobody unless the issue is routed.
- Show a screenshot inline with `--image` on `message`, `comment` or `ask` ([Documents](skill://dispatch/references/documents.md#artifacts)).
- Answer BTW, Aside, Steer and Agents-page messages as [messages.md](skill://dispatch/references/messages.md) says.

## Following

You follow each ask you open or reply to. Replies to your comments and new comments on documents you wrote also reach you. Nothing else on an issue reaches you unless you subscribe ([asks.md](skill://dispatch/references/asks.md#following)).

## References

Link every reference: an issue, ask, comment, spec or message ([reference forms](skill://dispatch/references/reading.md#reference-forms)), or an artifact. Link an eval by its viewer URL, a Slack message by its permalink, a Drive file by its share link. Never write "see this eval", "the comment above" or "per the spec" without a link. If a thing has no address, say so.

---
name: dispatch-writing-specs
description: "Use in a session with Dispatch before you write or change a spec, ask, comment or message a person reads. It replaces superpowers' brainstorming there."
---

# Writing specs

## The aim

Reach shared understanding with the person at the least cost of their time. A Legion phase worker writes none (`skill://legion-worker`).

## The outline

Find the issue's project with `dispatch projects` or `dispatch search`, never the environment. The spec's `##` parts, in order:

Part | Content
--- | ---
`Summary` | The change in the reader's own terms, at most 100 words
`Problem` | What goes wrong, for whom, with counts
`Goals and non-goals` | What is in and out of scope
`Requirements` | a `:::requirement` block each, `**R1.**` on, its source (`From:`)
`Verification` | the issue's `verification.md`, a `:::check` block each: `serves`, `surface`, `run`, `when`, `negative_control`
`Design` | How it meets them; a value and its reason for each limit, size and time
`Decisions` | The spec's decision nodes and answered ask blocks, at the section each shaped

`serves` and `needs` name a block of this issue or an ancestor. Another agent can build and check the work from these alone. A small change gets a few sentences per part, no matrix; split a large design into child issues.

## Rules for the text

For each spec, ask, comment and message:

- Write in `skill://asd-ste100`'s strict mode.
- Write about the current design. Keep a person's answered ask in its spec, with the answer unchanged; do not restate or rewrite their words as an agent decision.
- Link each source where the person wrote it, each id whole. With no link, state the design.
- In a spec, cite code by file and symbol, never a line number: it goes stale at the next edit.
- A sentence that cites a gate passing (test, review, check) also says what the pass does not show: "Tests pass: a wrong verdict, not a missing one."
- Name your session only if a reader must reach it.
- Keep only text that helps a reader understand, decide, build, check.
- Explain each name or term a new reader may not know, and what each issue you name does for them.
- In an ask, ask block, comment or message, name a requirement or check by what it is, never its number.

## Questions

- Name the concrete case first: which machines, files, sessions or people, and the numbers. Then ask the choice in those terms, never "Which option?".
- A choice inside what the person asked for, or one their messages favour, is yours: decide it.
- Ask only for the person's authority, taste or risk appetite, when options differ on what they care about.
- Ask with an [ask block](skill://dispatch/SKILL.md#asks-and-ask-blocks): 2 to 4 options with costs, your recommendation and its reason, answerable on a phone.

## Each version

1. After a person's answer, comment or edit, rewrite what it touches and delete what it contradicts. Never narrate an edit or what used to be here: versions and comments hold the history. Reply in a comment's thread.
2. Before each spec write, paste the whole spec, never a path, into a fresh subagent: it lists gaps, conflicts, uncheckable requirements, unclear text and missing or needless questions. Fix or record each.
3. Before each approval request, `wait` for a read-only fact check: `task(agent="scout")`.
4. Name each version with `dispatch doc-edit --summary`.

## Decisions

An answered ask is the decision node itself. Keep it in the spec, in its section, with the person's selected options and answer unchanged. Never remove or move a person's answered decision out of a spec.

For a choice you made yourself, write a `:::decision` block in the spec with `title` naming the choice. Its body gives your reason and the options you weighed. The Decisions tab labels it as the agent's choice.

- Set `shaped="dispatch://KEY/spec#b-<id> ..."` on the decision or ask node for each spec block it shaped (`doc-read --blocks` lists the ids; write ids percent-encoded).
- A choice derived from an ask may set `from_ask="dispatch://KEY/ask/<id>"` for provenance. That does not turn the agent's choice into the person's answer.
- `decided_by` is a canonical person login, when the decision genuinely belongs to that person. It is not a place for an agent's interpretation.
- Add an `Unsure:` paragraph for what would change your mind. Set `replaced="true"` when the agent's choice no longer applies; retain the node and its identity.

## Old specs

At your next edit, rewrite prose outside the outline in one version. Retain every answered ask in its section with its identity and the person's answer unchanged. Move only open asks with `doc-edit`'s `move`; do not turn a person's answered decision into an agent's summary.

## Approval

- Request approval of the spec and `verification.md` (`--include-verification`): each ask block answered or waived, each claim checked, this version unapproved. An unrun check never delays it.
- After approval, version only a change to scope, a check, cost, risk or a person's decision, all at once; put other fixes in the plan or a comment. An exact undo can restore approval.

## The plan

Write the plan with `writing-plans` as the issue's `plan.md`, not a repository file. Each task is a `:::plan_task` block with `serves` and `needs`.

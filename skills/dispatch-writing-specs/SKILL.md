---
name: dispatch-writing-specs
description: "Use in a session with Dispatch before you write or change a spec, and before an ask, comment or message a person reads. It replaces superpowers' brainstorming there."
---

# Writing specs

## The aim

Reach shared understanding with the person at the least cost of their time. A Legion phase worker writes no spec (`skill://legion-worker`).

## The outline

Find the issue's project with `dispatch projects` or `dispatch search`, never the environment. The spec's `##` parts, in order:

Part | Content
--- | ---
`Summary` | The change in the reader's own terms, at most 100 words
`Problem` | What goes wrong, for whom, with counts
`Goals and non-goals` | What is in and out of scope
`Requirements` | `**R1.**` on, each naming its source (`From:`)
`Design` | How it meets them; a value for each limit, size and time
`Decisions` | Your own choices
`Acceptance` | `**A1.**` on, each naming the requirements it tests

Another agent can build and check the work from the spec alone. A small change gets a few sentences per part, no matrix. Split a large design into child issues.

## Rules for the text

For each spec, ask, comment and message:

- Write in `skill://asd-ste100`'s strict mode.
- Write about the current design, never the person or how people reached it. Never quote people or upload their words.
- Link each source where the person wrote it, each id whole. With no link, state the design.
- In a spec, cite code by file and symbol, never a line number: it goes stale at the next edit.
- A sentence that cites a gate passing (a test, review, check or proof) also says what the pass does not show: "The tests pass; they catch a wrong verdict, not a proof of a contradiction."
- Name your session only if a reader must reach it.
- Keep only text that helps a reader understand, decide, build, check.
- Explain each name or term a new reader may not know. Say in a clause what each issue you name does for this reader.
- In an ask, ask block, comment or message, name a requirement or check by what it is, never its number.

## Questions

- Name the concrete case first: which machines, files, sessions or people, and the numbers. Then ask the choice in those terms, never "Which option?": a reader cannot work back from a general question to the case.
- Propose an answer to each point, numbers included, with a reason.
- A choice inside what the person asked for, or one their messages favour, is settled: decide it.
- Ask only for the person's authority, taste or risk appetite, when options differ on what they care about.
- Ask with an [ask block](skill://dispatch/SKILL.md#asks-and-ask-blocks): 2 to 4 options with costs, your recommendation, answerable on a phone.

## Each version

1. After a person's answer, comment or edit, rewrite what it touches; delete what it contradicts. Never narrate an edit or what used to be here; versions and comments hold the history. Reply in a comment's thread.
2. Before each spec write, paste the whole spec, never a path, into a fresh subagent's assignment. It lists gaps, conflicts, uncheckable requirements, unclear text, and missing or needless questions. Fix or record each.
3. Before each approval request, a read-only agent checks its facts: `task(agent="scout")`.
4. Name each version with `dispatch doc-edit --summary`.

## Decisions

- Each design choice no source states is a `###` entry: choice, reason, options weighed, parts shaped.
- An unsure entry adds an `Unsure:` paragraph: what would change your mind.
- A replaced entry moves under a final `### Replaced`.
- Link a person's answered ask block from each part it shapes.

## Old specs

At your next edit, rewrite a spec outside the outline whole, in one version. Move its ask blocks with `doc-edit`'s `move`. Kept text follows the text rules: restate each retelling, quotation or unlinked attribution as design, or cut it.

## Approval

- Request approval once, when each ask block is answered or waived, each claim is checked, and this version is unapproved. Unbuilt acceptance checks never delay it.
- After approval, version only a change to scope, acceptance, cost, risk or a person's decision, all at once. Put other fixes in the plan or a comment.

## The plan

Write the plan with `writing-plans` as the issue's `plan.md`, not a repository file.

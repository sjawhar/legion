---
name: dispatch-writing-specs
description: "Use in a session with Dispatch before you write or change a spec, and before an ask, comment or message a person reads. It replaces superpowers' brainstorming there. A Legion architect also follows skill://legion-architect."
---

# Writing specs

## The aim

Reach a shared understanding with the person, at the least cost of their time and attention. A Legion phase worker writes no spec: it sends each decision to its architect (`skill://legion-worker`).

## The outline

The spec has seven `##` parts, in order:

Part | Content
--- | ---
`Summary` | The change, in at most 100 words
`Problem` | What goes wrong, for whom, with counts
`Goals and non-goals` | What it does and does not do
`Requirements` | `**R1.**` on, each naming its source (`From:`)
`Design` | How it meets them
`Decisions` | Your own choices
`Acceptance` | `**A1.**` on, each naming the requirements it tests

Another agent can build the work and check each requirement from the spec alone. Split a design too big for one spec into child issues.

## Rules for the text

For each spec, ask, comment and message:

- Write in the strict mode of `skill://asd-ste100`.
- State the current design, never how people reached it: the reader is often the person who spoke.
- Link each source where the person wrote it: `dispatch://KEY/comment/<id>`, `/ask/<id>`, `/message/<id>`, or a Slack URL. With no link, state the design.
- Never quote or retell people, tell them what they said, or repeat an answered ask. Never upload people's words as a document.
- Name your session only if the reader must reach it.
- Keep only text that helps the reader understand, decide, build, check.
- Explain each term a reader may not know, and give each issue key its title, where it first appears.
- In an ask, comment or message, say what a requirement or check is, never its number.

## Questions

- Propose an answer to each point, numbers included, with a reason.
- Ask only for the person's authority, taste or risk appetite, when the options differ in a way they care about. Decide the rest, under "Decisions".
- Ask with an ask block at the end of its section ([Asks and ask blocks](skill://dispatch/SKILL.md#asks-and-ask-blocks)): 2 to 4 options, each one's cost, and your recommendation.
- A person does not watch your chat: reach them by an ask or approval request.

## Each version

1. After each answer, comment or edit by a person, rewrite each part it touches and delete what it contradicts.
2. Before each version, the first too, give a fresh subagent the spec alone: no path, code or history. It lists gaps, conflicts, duplicates, uncheckable requirements, unexplained terms, unclear sentences, and missing or needless questions. Fix each, or record it under "Decisions".
3. Before each approval request, a read-only agent checks the spec's facts against the code and the person's messages: `task(agent="scout")`, or `plan-gap-analyst` in Legion. Save command output to files for it.
4. Name each version with `dispatch doc-edit --summary`.

## Decisions

- Each choice you made alone is a `###` entry: the choice, the reason, the options weighed, and the parts it shaped.
- An unsure entry adds a paragraph starting `Unsure:`: what would change your mind.
- A replaced entry moves under `### Replaced`, at the end.
- A person's decision is their answered ask block. Each part it shapes links it.

## Old specs

Rewrite a spec outside the outline whole, in one version, when you next edit it. Move its ask blocks with `dispatch doc-edit`'s `move`.

## Approval

- Request approval only when each ask block is answered or waived, the person agreed to each point, and this version is not approved.
- `--summary` says in one to three sentences what the person approves.
- When their answer is the whole change since the approved version, Dispatch carries the approval: request nothing.
- A Legion root spec's design gate follows `skill://legion-architect`.

## The plan

After approval, write the plan with `writing-plans` as the issue's `plan.md` (`dispatch artifact --issue <KEY> --name plan.md --content-file -`), not in the repository.

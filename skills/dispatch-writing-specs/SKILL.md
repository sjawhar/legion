---
name: dispatch-writing-specs
description: "Use in a session with Dispatch before you write or change a spec, and before an ask, comment or message a person reads. It replaces superpowers' brainstorming there. A Legion architect also follows skill://legion-architect."
---

# Writing specs

## The aim

Reach a shared understanding with the person, at the least cost of their time and attention. A Legion phase worker writes no spec: it sends each decision to its architect (`skill://legion-worker`).

## The outline

The spec is the issue's primary document. Find its project with `dispatch projects` or `dispatch search`, never the environment. Its seven `##` parts, in order:

Part | Content
--- | ---
`Summary` | The change, in at most 100 words
`Problem` | What goes wrong, for whom, with counts
`Goals and non-goals` | What is in and out of scope
`Requirements` | `**R1.**` on, each naming its source (`From:`)
`Design` | How it meets them
`Decisions` | Your own choices
`Acceptance` | `**A1.**` on, each naming the requirements it tests

Another agent can build and check the work from the spec alone. Split a design too big into child issues.

## Rules for the text

For each spec, ask, comment and message:

- Write in `skill://asd-ste100`'s strict mode.
- State the current design, never how people reached it.
- Link each source where the person wrote it: `dispatch://KEY/comment/<id>`, `/ask/<id>`, `/message/<id>`, or a Slack URL. With no link, state the design.
- Never quote or retell people, tell them what they said, repeat an answered ask, or upload their words as a document.
- Name your session only if a reader must reach it.
- Keep only text that helps the reader understand, decide, build, check.
- Explain each term a reader may not know, and give each issue key its title, where it first appears.
- In an ask, comment or message, say what a requirement or check is, never its number.

## Questions

- Propose an answer to each point, numbers included, with a reason.
- A choice inside what the person asked for is settled: build it, ask nothing.
- Ask only for the person's authority, taste or risk appetite, when options differ on what they care about.
- Ask with an [ask block](skill://dispatch/SKILL.md#asks-and-ask-blocks) at the end of its section: 2 to 4 options with costs, your recommendation, answerable on a phone.
- A person does not watch your chat: reach them only by an ask or approval request.

## Each version

1. After a person's answer, comment or edit, rewrite what it touches and delete what it contradicts. Reply in the comment's thread; resolve the ask a comment answers.
2. Before each version, the first too, a fresh subagent gets the spec alone and lists gaps, conflicts, duplicates, uncheckable requirements, unclear text, and missing or needless questions. Fix or record each.
3. Before each approval request, a read-only agent checks its facts against the code and the person's messages (`task(agent="scout")`; `plan-gap-analyst` in Legion).
4. Name each version with `dispatch doc-edit --summary`.

## Decisions

- Each choice you made alone is a `###` entry: choice, reason, options weighed, parts shaped.
- An unsure entry adds an `Unsure:` paragraph: what would change your mind.
- A replaced entry moves under a final `### Replaced`.
- A person's decision is their answered ask block. Each part it shapes links it.

## Old specs

Rewrite a spec outside the outline whole, in one version, at your next edit. Move its ask blocks with `doc-edit`'s `move`.

## Approval

- Request approval only when each ask block is answered or waived, the person agreed to each point, and this version is unapproved.
- When their answer is the whole change since approval, Dispatch carries the approval.
- After approval, make a new version only for a change to scope, acceptance, cost, risk or a decision the person made, and put all such changes into one version. Put any other fix in the plan or a comment.
- A Legion root spec's design gate follows `skill://legion-architect`.

## The plan

Write the approved spec's plan with `writing-plans` as the issue's `plan.md`, not a repository file.

---
name: dispatch-writing-specs
description: "Use with Dispatch before writing a spec or a person-facing ask, comment or message. It replaces superpowers' brainstorming; a Legion architect also follows skill://legion-architect."
---

# Writing specs

## The aim

Write for shared understanding at low reader cost. A Legion phase worker sends decisions to its architect (`skill://legion-worker`).

## The outline

The issue spec is its primary document. Find its project through Dispatch. Its `##` parts:

Part | Content
--- | ---
`Summary` | The change in the reader's own terms, at most 100 words
`Problem` | What goes wrong, for whom, with counts
`Goals and non-goals` | What is in and out of scope
`Requirements` | `**R1.**` on, each naming its source (`From:`)
`Design` | How it meets them; a value for each limit, size and time
`Decisions` | Your own choices
`Acceptance` | `**A1.**` on, each naming the requirements it tests

A builder can implement and check from the spec. Keep a small change to a few sentences per part; split a large design into child issues.

## Rules for the text

For each spec, ask, comment and message:

- Write in `skill://asd-ste100`'s strict mode.
- State the current design, never how people reached it.
- Link each source: `dispatch://KEY/comment/<id>`, `/ask/<id>`, `/message/<id>`, or Slack URL. Without a link, state the design.
- Write about the design, not people. Do not quote people or repeat answered asks.
- Name your session only if a reader must reach it.
- Keep only text that helps a reader understand, decide, build, check.
- Explain unfamiliar terms. Name an issue by its `dispatch://` link and its reader-relevant purpose. Never write a bare key.
- In an ask, ask block, comment or message, name a requirement or check by what it is, never its number.

## Questions

- Propose an answer to each point, numbers included, with a reason.
- A choice inside what the person asked for, or one their messages favour, is settled: decide it.
- Ask only for the person's authority, taste or risk appetite, when options differ on what they care about.
- Ask with an [ask block](skill://dispatch/SKILL.md#asks-and-ask-blocks) at the end of its section: 2 to 4 options with costs, your recommendation, answerable on a phone.
- Chat reaches nobody: use an ask or approval request.

## Each version

1. After a person's answer, comment or edit, rewrite what it touches; delete what it contradicts. Write what is true now. Never narrate an edit or what used to be here; versions and comments hold the history. Reply in a comment's thread; resolve the ask it answers.
2. Before each spec write, paste the whole spec, never a path, into a fresh subagent's assignment. It lists gaps, conflicts, uncheckable requirements, unclear text, and missing or needless questions. Fix or record each.
3. Before each approval request, a read-only agent checks its facts: `task(agent="scout")`, or `plan-gap-analyst` and `plan-reviewer` in Legion.
4. Name each version with `dispatch doc-edit --summary`.

## Decisions

- Each design choice no source states is a `###` entry: choice, reason, options weighed, parts shaped.
- An unsure entry adds an `Unsure:` paragraph: what would change your mind.
- A replaced entry moves under a final `### Replaced`.
- A person's decision is their answered ask block. Each part it shapes links it.

## Old specs

At your next edit, rewrite a spec outside the outline whole, in one version. Move its ask blocks with `doc-edit`'s `move`.

## Approval

- Request approval once, when each ask block is answered or waived, each claim is checked, and this version is unapproved. Unbuilt acceptance checks never delay it.
- When their answer is the whole change since approval, Dispatch carries it.
- After approval, version only a change to scope, acceptance, cost, risk or a person's decision, all at once. Put other fixes in the plan or a comment.
- A Legion root spec's design gate follows `skill://legion-architect`.

## The plan

Write the approved spec's plan with `writing-plans` as the issue's `plan.md`, not a repository file.

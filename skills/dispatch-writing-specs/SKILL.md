---
name: dispatch-writing-specs
description: "Use in a session with Dispatch before you write or change a spec, ask, comment or message a person reads. It replaces superpowers' brainstorming there."
---

# Writing specs

## The conversation

Reach shared understanding at the least cost of the person's time. A spec is a conversation, not a document you write and then ask about. A Legion phase worker writes none (`skill://legion-worker`). In order:

1. Read the issue, the code and what the person wrote.
2. Send what you found and the questions it leaves open, a few at a time, before the spec states a design.
3. Write each part only after the answers it rests on.
4. A wrong premise or a changed request changes the request first.

## The outline

Find the issue's project with `dispatch projects` or `search`, never the environment. The spec's `##` parts, in order:

Part | Content
--- | ---
`Summary` | The change in the reader's terms, at most 100 words
`Problem` | What goes wrong, for whom, with counts
`Goals and non-goals` | What is in and out of scope
`Requirements` | a `:::requirement` block each, `**R1.**` on, its source (`From:`)
`Verification` | the issue's `verification.md`, a `:::check` block each: `serves`, `surface`, `run`, `when`, `negative_control`
`Design` | How it meets them; a value and its reason for each limit, size and time
`Decisions` | One line: `The decisions are in dispatch://KEY/artifact/decisions-md.`

`serves` and `needs` name a block of this issue or an ancestor. Another agent can build and check the work from these alone. A small change gets a few sentences per part, no matrix; split a large design into child issues.

## Rules for the text

For each spec, ask, comment and message:

- Write in `skill://asd-ste100`'s strict mode.
- Write about the current design, never the person or how people reached it, and never quote or upload their words.
- Link each source where the person wrote it, each id whole. With no link, state the design.
- In a spec, cite code by file and symbol, never a line number: it goes stale at the next edit.
- A sentence citing a gate passing (test, review, check) also says what the pass does not show: "Tests pass: a wrong verdict, not a missing one."
- Keep only text a reader needs to understand, decide, build or check, and say what each issue you name does for them (`skill://plain-words`).
- In an ask, ask block, comment or message, name a requirement or check by what it is, never its number.

## Questions

- Name the concrete case and its numbers, then the choice in those terms, never "Which option?".
- A choice inside what the person asked for, one their messages favour, routine cleanup, sequencing, naming, a choice between equivalent options, and a fix you can make are yours.
- Ask only for the person's authority, taste or risk appetite, when options differ on what they care about.
- Ask with an [ask block](skill://dispatch/SKILL.md#asks-and-ask-blocks): 2 to 4 options, answerable on a phone, and name nothing the reader cannot see from it ("the message above").
- For each technical term you coined in this session, count by exact match the files holding it, in the repositories the work touches and in Dispatch; a single-digit count means expand the term or drop it, because a coinage reads as familiar to you. A count misses a coinage of common words, so read the draft for those too.
- Measure each claim the ask rests on, its premise and each option's precondition. Where you cannot, mark the claim unverified and name the missing measurement; call an option unavailable only where a precondition demonstrably fails. A premise can go stale within the hour; no unmeasured number goes in one.
- Before you pass a question on as one only an attended or privileged act answers, name the source that answers it and try it, then the other records holding it, because a 403 or an unfamiliar surface does not make a fact unknowable. Keep a privileged read for what only the live system knows.

## Each version

1. After a person's answer, comment or edit, reread the problem and rewrite what the answer touches, deleting what it contradicts: an answer is integrated, never appended. Answer each comment in its thread. Never narrate an edit: versions and comments hold the history.
2. Before each spec write, paste the whole spec, never a path, into a fresh subagent to list gaps, conflicts, uncheckable requirements, unclear text and needless questions. Fix or record each.
3. Before each approval request, `wait` for a read-only fact check: `task(agent="scout")`.
4. Name each version with `dispatch doc-edit --summary`.

## Decisions

Choices—answered asks included—earn `decisions.md` `##` sections only if a disagreeing reader would act differently; keep others to one spec line each.

- The heading names the choice; the text gives the reason and the options weighed.
- Link each spec block it shaped, `dispatch://KEY/spec#b-<id>`, and a person's decision its ask, `dispatch://KEY/ask/<id>`.
- An unsure section adds a paragraph starting `Unsure:`: what would change your mind.
- A replaced section moves under a final `## Replaced` as a `###`.

## Old specs

Rewrite a spec outside the outline whole in one version, its ask blocks moved with `doc-edit`'s `move`; restate each retelling, quotation or unlinked attribution as design, or cut it.

## Approval

- Request approval of the spec and `verification.md` (`--include-verification`): each ask block answered or waived, each claim checked, this version unapproved. An unrun check never delays it.
- After approval, version only a change to scope, a check, cost, risk or a person's decision, all at once; put other fixes in the plan or a comment. An exact undo can restore approval.

## The plan

Write the plan with `writing-plans` as the issue's `plan.md`, not a repository file. Each task is a `:::plan_task` block with `serves` and `needs`.

---
name: dispatch-first
description: "Use when a session has the dispatch command, before you plan, file an issue, ask a human, post a finding, or start work. Someone may track that work already or hold a decision on it."
---

# Dispatch first

Dispatch holds the open issues, their specs and people's answers. A duplicate issue or repeated question costs a person time.

## Search before you act

- Before you plan, file an issue, ask, post a finding or start work, run `dispatch search`.
- Each query word must match. Use two or three words: the thing and what is wrong with it. Never paste a draft.

```bash
dispatch search --query 'broadcast send order'
dispatch search --query 'reviewer threads OR review comments'
```

- No hits: remove a word before you add one. Try two or three wordings before you decide nothing exists.
- Open each hit that can be yours with `dispatch read --issue KEY`, then its parent (`child_of` under `Links:`, or a document's `Issue:`).

## What to do with what you find

`dispatch issue` and `dispatch ask` also search. Treat a hit in `advice.suggestions` as yours to read.

- **Work already tracked:** put the finding on that issue. File a new issue only when no hit covers the work, and cite the nearest one you ruled out (`dispatch://KEY`).
- **Question already decided:** the answered ask is the record. Cite it (`dispatch://KEY/ask/<id>`). Do not ask again.
- **Duplicate found:** keep the issue with the spec, copy to it what only the duplicate has, and close the duplicate (`--status done --reason 'Duplicate of dispatch://KEY.'`). When it has the `legion` label or another holder's claim, comment the kept issue's name on it instead.
- **Each ask and message stands alone:** the reader sees only that text. Put the facts, options and your recommendation in it.
- In a Legion session, a finding outside your own issue is never an issue you file: follow `skill://legion-worker/references/findings.md`.

## Reach a person only through Dispatch

A person does not watch your chat. Reach them through an ask, an ask block or an approval request.

- The moment a person blocks your work, open an ask: an ask block for a design question its document records, else `dispatch ask`. A blocker that is not an open ask does not exist for them. A Legion phase worker sends decisions to its architect instead. Before you say you wait on a person, run `dispatch open-asks`.
- Record every request you accept in Dispatch before you start it: an issue, or a line in your issue's `open-items.md` artifact. Before you end a turn, read it. Each item is done, in progress, or an open ask. Never drop one.

## Which skill to read

| Before you | Read |
| --- | --- |
| run your first `dispatch` command in this session | `skill://dispatch` |
| write or change a spec, or write an ask, comment or message a person reads | `skill://dispatch-writing-specs` (in a session with Dispatch it replaces superpowers' `brainstorming`) |
| plan work whose spec a person approved | superpowers' `writing-plans`, stored as the issue's `plan.md` (`skill://dispatch-writing-specs`, "The plan") |
| write a Legion architect's own spec | `skill://legion-architect` too |

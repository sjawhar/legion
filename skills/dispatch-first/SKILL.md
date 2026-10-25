---
name: dispatch-first
description: "Use when a session has the dispatch command, before you plan, file an issue, ask a human, post a finding, or start work. Someone may track that work already or hold a decision on it."
---

# Dispatch first

Dispatch holds the open issues, their specs and people's answers. A duplicate issue or a repeated question costs a person time and splits the history.

## Search before you act

- Before you plan, file an issue, ask, post a finding or start work, run `dispatch search`.
- Each query word must match, so each added word can only lose hits. Use two or three words, the thing and what is wrong with it. Never paste a draft.

```bash
dispatch search --query 'broadcast send order'
dispatch search --query 'reviewer threads OR review comments'
```

- No hits: remove a word before you add one. Try two or three wordings (the component, the symptom, the fix) before you decide nothing exists.
- Open each hit that can be yours with `dispatch read --issue KEY`, then its parent (the `child_of` row under `Links:`, or a document's `Issue:`). Its children and `Components:` line show where the rest of the work is.

## What to do with what you find

`dispatch issue` and `dispatch ask` also search. A hit in the result's `advice.suggestions` is a hit you found.

- **Work already tracked:** put the finding on that issue. File a new issue only when no hit covers the work, and cite the nearest one you ruled out (`dispatch://KEY`).
- **Question already decided:** the answered ask is the record. Cite it (`dispatch://KEY/ask/<id>`). Do not ask again.
- **Duplicate found:** keep the issue with the spec and discussion, and copy to it what only the duplicate has. Close the duplicate: `dispatch issue-update --issue <KEY> --status done --reason 'Duplicate of dispatch://KEY.'`. When it has the `legion` label, or someone else holds its claim, comment on it with the kept issue's name instead.
- **Each ask and message stands alone:** the reader sees only that text. Put the facts, the options and your recommendation in it.
- In a Legion session, file no issue for a finding outside your issue: follow `skill://legion-worker/references/findings.md`.

## Reach a person only through Dispatch

A person does not watch your chat. To reach a person, open an ask, write an ask block in the spec, or request approval.

## Which skill to read

| Before you | Read |
| --- | --- |
| run your first `dispatch` command in this session | `skill://dispatch` |
| write or change a spec, or write an ask, comment or message a person reads | `skill://dispatch-writing-specs` (in a session with Dispatch it replaces superpowers' `brainstorming`) |
| plan work whose spec a person approved | superpowers' `writing-plans`, stored as the issue's `plan.md` (`skill://dispatch-writing-specs`, "The plan") |
| write a Legion architect's own spec | `skill://legion-architect` too |

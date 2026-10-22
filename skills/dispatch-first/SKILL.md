---
name: dispatch-first
description: "Use when a session has the dispatch command, before you plan, file an issue, ask a human, post a finding, or start work. Someone may track that work already or hold a decision on it."
---

# Dispatch first

Dispatch holds the open issues, their specs and the answers humans gave. A duplicate issue or a repeated question costs a human time and splits the history.

## Search before you act

- Before you plan, file an issue, ask, post a finding or start work, run `dispatch search` (`dispatch --help` lists every command).
- Every query word must match, so each added word can only lose hits. Use two or three words: the thing and what is wrong with it, as a user names them. Never paste a draft.

```bash
dispatch search --query 'broadcast send order'
dispatch search --query 'reviewer threads OR review comments'
```

- No hits: drop a word before you add one. Try two or three wordings (the component's name, the symptom, the fix) before you conclude that nothing exists.
- Open each hit that could be yours with `dispatch read`, then its parent (the `child_of` row under `Links:`; a document hit names its issue on `Issue:`). The parent's children and the `Components:` line show where the rest of the work is.

## What to do with what you find

`dispatch issue` and `dispatch ask` also search: treat a hit in the result's `advice.suggestions` as one you found.

- **Work already tracked:** put the finding on that issue (a comment, a message, or a spec edit). File a new issue only when no hit covers the work, and cite the nearest one you ruled out (`dispatch://KEY`).
- **Question already decided:** the answered ask is the record. Cite it (`dispatch://KEY/ask/<id>`) where you rely on it. Do not ask again.
- **Duplicate found:** keep the issue with the spec and the discussion, and copy over what only the duplicate has. Then close it: `dispatch issue-update --issue <KEY> --status done --reason 'Duplicate of dispatch://KEY.'`. If it has the `legion` label, or another session or a human holds its claim, comment on it with the survivor's name. Do not close it.
- **Every ask and message stands alone:** the reader sees only that text. Put the facts, the options and your recommendation in it. Link what you cite (`dispatch://…`). Never write "see above" or "my earlier message".

## Design in the spec

- A design conversation with a person, or the plan that follows it, uses `skill://dispatch-brainstorming`. Read it before your first design question, the spec, or the plan. It replaces superpowers' `brainstorming` and `writing-plans` here, also when the user names one of them.
- Before a spec's first version and each approval request, get a fresh read-only subagent to check it. On Oh My Pi, use `task(agent="scout")`. Elsewhere, use the host's read-only agent. It checks the spec against the code, the human's words and the command output that its claims rest on. Scout has no shell: save that Dispatch, gh and git output to files first and give it their paths. Resolve what it flags.
- A Legion architect's own issue spec follows `skill://legion-architect`. Its fresh reader in that Legion session is the Legion plugin's `plan-gap-analyst` agent.

## Load the full skill before you write

Before any write to Dispatch, load `skill://dispatch`: an ask, a message, a comment, a document edit, a status change, a claim. Skip this if you loaded it in this session.

---
name: dispatch-first
description: "Use in any session with Dispatch tools, before planning, filing an issue, asking a human, posting a finding, or starting work that someone may already track or have decided."
---

# Dispatch first

Dispatch already holds most of what you are about to plan, file or ask: the open issues, their
specs, and the answers humans gave. A second issue for tracked work, or a question a human already
answered, costs that human the time to notice it and splits the history across two places.

## Search before you act

Before you plan, file an issue, ask, post a finding or start work, search Dispatch. Every word of
the query must match, so each word you add can only lose hits: search with two or three words, the
thing and what is wrong with it, as a user would name them. Never paste a draft.

```ts
dispatch_search({ query: "broadcast send order" })
dispatch_search({ query: "reviewer threads OR review comments" })
```

When a query finds nothing, drop a word before you add one. Try two or three wordings (the
component's name, the symptom, the fix) before you conclude that nothing exists. Open every hit
that could be yours with `dispatch_read`, then its parent (the `child_of` row under `Links:`); the
parent's children and the issue's `Components:` line show where the rest of that work lives.

## What to do with what you find

- **The work is already tracked: extend that issue.** Put the finding on it (a comment, a message,
  or an edit to its spec) instead of filing another. File a new issue only when no hit covers the
  work, and cite the nearest one you ruled out (`dispatch://KEY`).
- **The question is already decided: cite the decision.** An answered ask is the record. Point to
  it where you rely on it (`dispatch://KEY/ask/<id>`) instead of asking again.
- **You met a duplicate: close it.** Keep the issue that holds the spec and the discussion, carry
  over anything only the duplicate has, then close the duplicate with a reason that names the
  survivor: `dispatch_issue_update({ issue, status: "done", reason: "Duplicate of dispatch://KEY." })`.
  When the duplicate carries the `legion` label, or another session or a human holds its claim,
  comment on it naming the survivor instead of closing it.
- **Every ask and message stands on its own.** The person who answers sees only that text: put the
  facts, the options and your recommendation in it, and link what you cite (`dispatch://…`)
  instead of writing "see above" or "my earlier message".

## Load the full skill before you write

- **Before you write or change a spec, read `skill://dispatch`,** including its "Writing a spec"
  section.
- Before any other write to Dispatch (an ask, a message, a comment, a document edit, a status
  change, a claim), load `skill://dispatch` unless you already have in this session.

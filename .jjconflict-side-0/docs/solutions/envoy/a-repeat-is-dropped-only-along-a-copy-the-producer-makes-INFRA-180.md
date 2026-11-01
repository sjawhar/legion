---
title: "A repeat is dropped only along a copy the producer makes"
category: envoy
tags:
  - envoy
  - normalization
  - slack
  - deduplication
  - untrusted-input
  - testing
date: 2026-10-25
status: active
module: envoy
related_issues:
  - "INFRA-180"
  - "sjawhar/legion#2166"
---

# A repeat is dropped only along a copy the producer makes

This is for code that copies an outside party's structured text — a Slack message's blocks and
attachments, a webhook's nested payload — into one field an agent reads. Gathering the strings is
the easy half. Deciding which of them are the same words twice is where the data is lost.

## The rule

1. **Before writing any repeat rule, write the copy table.** One row per copy the *producer*
   itself makes: which producer makes it, what is copied, and how far apart the two copies can
   sit. Compare only along a row, and only within that row's distance. Anything not in the table
   is two different words that happen to match.
2. **Never drop a string for resembling another.** Containment deletes whole values — a field
   value `1` disappears because an unrelated line reads `incident 4213`. Equality across the whole
   message deletes them too — two hosts both reading `ok`, a second count of `0`. Both read as a
   rule about strings; neither is a rule about copies.
3. **Where the rule must be lossy, make it duplicate rather than lose, and record the duplicate as
   a trade.** For an agent reader an extra line is cheap and a missing `0` is wrong data.
4. **Deliver in the renderer's order, under the renderer's names for what it shows.** The first
   line delivered becomes the summary, so order is part of the contract. A string under a name the
   renderer does not display (`type`, `image_url`, a colour) is not a word a person reads.
5. **Use one normalisation of a source for every comparison against it.** Two forms of the same
   text — one escaped, one decoded — in two comparisons is a repeat the rule will miss.
6. **Prove it with a corpus, not with a reading.** One body per producer kind (the vendor's own
   client, a modern layout app, a legacy-attachment app, an edit), plus short values — `0`, `1`,
   `ok`, a host name — placed both inside a token of the message's own text and repeated across
   sibling objects. For each body write by hand the strings a person sees rendered, in order, then
   diff that against what is delivered into two columns: **lost** (rendered, not delivered) and
   **duplicated** (delivered twice, or never rendered). A non-empty *lost* column is a defect; the
   *duplicated* column is the trade of rule 3, to be recorded. A table test over hand-written
   expectations computes this; a live listener then confirms the wiring and nothing more.
7. **Make each clause of the rule fail a case of its own.** Delete the clause in a scratch copy and
   require exactly one named case to fail. A clause whose case is decided by an earlier clause is
   not pinned, and a later refactor reverts it with CI green.
8. **Read the producer's own authoring guidance before choosing which field to trust.** What an app
   is told to put in each field decides which field may be ignored, and it is written down.

## Evidence

INFRA-180 gave Envoy's Slack normalisation the words a message carries in its `attachments` and
`blocks`, since an app that formats a message leaves `text` empty and woke an agent with a summary
ending in `: ` and a payload holding no words at all. The gathering was right in the first round.
The repeat rule took four review rounds, each found by driving a real Slack-shaped body through a
real listener, and each one a different scope for the same question:

| Round | Rule tried | What it lost |
| --- | --- | --- |
| 1 | skip a piece the collected text already **contains** | `Severity`'s value `1` and `Hosts down`'s `2`, because an own-text line read `incident 4213`; the same message with `incident 9999` kept them |
| 2 | **exact equality** message-wide, plus: ignore a `rich_text` block when the message has words of its own | an app's own rich text — `FAIL TestFoo at line 42` beside a `text` of `Build failed` — which is the shape Slack's own guidance tells app authors to post |
| 3 | skip a `rich_text` object only when every string in it is already held by the own text | still message-wide for everything else: a CI alert's second `0`, and one of two attachments both reading `ok` |
| 4 | **scoped**: walk-wide only for the message's own text and its lines; per object for the strings an object shows under its own keys; and an attachment's `fallback` read last and dropped only when that attachment's own subtree already said it | the rule that shipped |

Round 4 is rule 1 applied: three comparisons, one per copy Slack actually makes — the client's
`rich_text` copy of the words the user typed, an attachment repeating itself across its own names,
and a `fallback` that is the plain-text copy of the attachment below it. The first three rounds
each compared strings at a scope wider than the copy they were aimed at.

Two more things the sequence showed:

- Round 2's wrong turn was decidable without code. Slack tells app authors to put the plain-text
  copy in `text` and the formatting in a `rich_text` block, so "ignore `rich_text` when `text` is
  non-empty" was contradicted by the producer's own documentation (rule 8).
- A whole extra round existed only because the acceptance case named for the repeat rule,
  `a repeated piece`, gave the message the same own text and was therefore decided by the
  walk-wide comparison before the per-object one was reached. Deleting the per-object condition
  left all 39 table cases green. Setting that fixture's own `text` to `""` makes exactly that case
  fail under the same deletion (rule 7).

Recorded against the shipped code at `packages/envoy/internal/contracts/normalize.go`, found by
the retro's fresh reader and not by any round, as an instance of rule 5: the walk-wide set is built
from the message's own text **as it arrives**, still carrying Slack's `&amp;`, `&lt;` and `&gt;`,
while the `rich_text` measurement compares against the same text **decoded**. A read `rich_text`
block whose segment copies an own line containing one of those three characters therefore repeats
it. That is a duplicate and not a loss, so it fits rule 3, and no case covers it. [INFERENCE: read
from the code during the retro, not driven.]

Surfaces that decided every round: a real `nats-server`, a real `envoy-listener` built from the
head under test, and a session receiving the wake, with the previous round's listener built from
its own commit and run on the same bodies as the before. A before-and-after that names both
commits is what made each round's claim checkable.

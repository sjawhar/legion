---
title: "A contract census enumerates from the gate's call graph, not from one constructor's grep"
category: best-practices
tags:
  - contract-change
  - census
  - enumeration
  - evidence
date: 2026-10-23
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "INFRA-66"
  - "sjawhar/legion#2087"
---

# A contract census enumerates from the gate's call graph, not from one constructor's grep

Extends docs/solutions/skill-patterns/a-ruling-that-reverses-a-prose-invariant-pins-the-old-phrases-absence-across-every-part-and-answers-nothing-enforces-x-with-a-classified-census-LEGION-631.md.
That document says a census answers with each search pattern, each hit and each hit's class
rather than a count. The delta here is where the pattern set comes from, which is exactly where
two censuses in a row went wrong.

- Enumerate from the refusing (or emitting) function's **call sites**; then, for each **input**
  of that function, enumerate every source that input has; then each source's callers.
- Write the instrument into the census, so the next reader can see what it structurally could not
  have found. A grep for one constructor finds one branch of a two-source function and yields a
  census that is confidently wrong in whichever direction that branch points.
- A measurement taken on a path that is **structurally immune** to the defect tests nothing. It
  is withdrawn from the claim, with its reason, not re-labelled.
- When a correction replaces a census, say what was wrong with the instrument, not only what the
  new command is. A corrected command with no account of the old one leaves the next table as
  likely to be wrong.

## Evidence

A change added one refusal that reaches an HTTP client. Three censuses of who could meet it:

1. Built from **where the error can surface** — the handler's error mapping — and claimed six
   route families could newly answer 503. Too much.
2. Built from `grep` for one constructor of the gate's cursor argument. That grep can only find
   one of the function's two sources, so the table listed three appends and concluded that an API
   write is never refused. Too little — and the sixteen concurrent cross-process edits offered as
   evidence for that claim ran on a path that pre-holds the document's lock from before its read,
   so it was structurally immune and had never tested the claim at all.
3. Built from the gate's call graph: its two call sites, then both branches of the cursor each
   passes it, then each branch's callers. Four surfaces, the fourth being a warm API write whose
   lock window another process appended in.

Both wrong censuses were written by someone who had just been shown the other mistake, which is
the argument for writing the instrument down rather than the result.

Related: docs/solutions/legion/a-claim-that-asserts-more-than-the-command-or-record-behind-it-produced-is-the-one-that-ships-INFRA-69.md
and docs/solutions/testing/a-command-that-cannot-fail-cannot-tell-you-which-half-answered-INFRA-69.md.

---
title: "Deleting a gate: verify each survivor's behaviour, not the removed symbol's absence — a helper the gate shared with a surviving rule keeps its whole normalisation, and a predicate stripped of its probe clauses must still discriminate"
category: legion
tags:
  - deletion
  - tool-call-hook
  - pane-rule
  - shared-helper
  - simplify-pass
  - e2e-predicate
date: 2026-10-08
status: active
module: packages/pi-legion
related_issues:
  - "LEGION-630"
  - "sjawhar/legion#1845"
---

# Deleting a gate: verify each survivor's behaviour, not the removed symbol's absence

- A plan sentence of the form "once gate G is gone, nothing can observe helper H" is a claim about
  H's callers, not about G. Before deleting or trimming H, list every caller of H (the language
  server's references, or `grep -n '\bH\b'` over the package) and read what each still needs from
  it. A caller that survives the deletion keeps H's full input normalisation, even the branch that
  was added for the deleted gate.
- The deletion's check is the survivor's behaviour on the input the deleted branch handled, pinned
  by one allowed case in the survivor's own test — not "the symbol no longer appears" (the census
  grep proves only that).
- When a proof predicate loses the clauses that observed the deleted behaviour, re-ask of each
  remaining conjunct what would make it false. A conjunct that was discriminating only because a
  probe sat next to it now holds for every run, and a predicate of such conjuncts proves nothing.
- The deletion skill's "almost entirely deletions" rule is a scope rule, not permission to skip
  this: the lines that keep a survivor whole are additions a deletion PR must carry.

## Evidence (LEGION-630, sjawhar/legion#1845)

The Legion extension's `tool_call` hook had a role gate (the architect's single-`legion`-command
bash rule; the architect's, reviewer's and merger's refusal of `edit`/`write`/`apply_patch`) and a
pane rule refusing `legion handoff complete` from a shell. Both went. The hook's remaining rule, the
LEGION-45 operation-log rule, reads a `write` to `proc://<id>` as a supervised service's stdin and
scans its content; it decides that through `nonFileWriteScheme`, which the deleted gate also used
(a `write` into Oh My Pi passed the mutation gate for every role).

Plan decision D6 read: "`PREFIXED_CONFLICT_URL` and `writeTarget`'s conflict branch existed only so
a disguised file write could not pass the role gate; with the gate gone no caller can observe
them." The premise was the gate's; the caller it missed was the operation-log rule. With the
branch deleted, a `write` to `proc://shell:conflict://2` — which Oh My Pi's `write` routes to the
workspace file `conflict://2` whatever the prefix (`recoverConflictUriPrefix`) — matched the
`proc://` scheme and its content was scanned, so a conflict-resolution write whose text mentioned
`jj undo` (this repository's skill and learnings do) would have been refused and never landed. The
plan review, the implementation and the unit suite all passed over it; the simplify pass's
code-quality reviewer found it by reading `nonFileWriteScheme`'s one remaining caller.

The fix kept the unwrap inside the survivor (`packages/pi-legion/extensions/legion.ts`,
`nonFileWriteScheme`: header unwrap, then the `conflict://` URL alone when a prefix stands before
it, then the scheme) and pinned it with one allowed case in `legion.test.ts`'s operation-log
stdin test: `write` `{ path: "proc://shell:conflict://2", content: "jj undo" }` returns
`undefined`. The removed symbol `writeTarget` stays removed; what the deletion was never allowed
to take was the normalisation.

The predicate case: `scripts/e2e/stage3-4b13b-acceptance.sh`'s stall proof lost its three
refusal clauses and kept `stall_at_instruction == "open"`. The reviewer noted that the conjunct is
now satisfied by the re-arm the script's own comment describes (Oh My Pi appends `open` for an
arriving Envoy message before the message's entry), so it no longer discriminates; the follow-up,
the `handoff_complete` call, the `closed` state and "no follow-up after done" carry the proof.
Recorded as a fast-follow; the rule above is what would have caught it at edit time.

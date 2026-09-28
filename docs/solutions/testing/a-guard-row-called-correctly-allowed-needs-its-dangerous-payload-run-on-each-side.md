---
title: "A guard row called correctly allowed needs its dangerous payload run on each side; a pattern count drops the name that looks like the others; value equality is provenance only where bash says so"
category: testing
tags:
  - pane-guard
  - security-control
  - boundary-testing
  - bash-semantics
  - field-enumeration
  - code-review
date: 2026-09-28
status: active
module: packages/pi-envoy
problem_type: testing
component: src/legion/pane-guard.ts
severity: high
applies_when:
  - A review says a guard's row is "correctly allowed", or that a declared over-refusal is narrower than stated
  - Counting or auditing an interface's fields, or checking that every site copying a state carries each one
  - Deciding whether a child changed something by comparing what it left with what it started from
related_issues:
  - "LEGION-121"
  - "sjawhar/legion#1536"
---

# A guard row called correctly allowed needs its dangerous payload run on each side

The pane guard (`packages/pi-envoy/src/legion/pane-guard.ts`) walks a command as bash would run
it and refuses a destructive one outside the pane's roots. Its review on sjawhar/legion#1536 ran
more than ten rounds. The last four each found a hole in ground the previous clean pass had just
covered. Three findings from them apply beyond the guard.

## 1. "Correctly allowed" is settled by the dangerous payload, not by the adjacent boundary

At `ffb7a984`, a file sourced with operands restored the caller's arguments afterwards when the
list it left equalled its operands by value. This was allowed:

```bash
set -- "$LEGION_WORKSPACE/a"; . lib.sh "$HOME/y"; rm -rf "$1"   # lib.sh: set -- "$@"
```

bash keeps a list a sourced file sets with `set --`, so `$1` is `$HOME/y` and the command deletes
it:

```
$ bash -c 'set -- SAFE; . ./reset.sh BAD; echo "$1"'    # reset.sh: set -- "$@"
BAD
```

Three reviewers and the coordinating session read that row, and all four called it correct
("correctly narrowed", "correctly allowed"). Each was checking the declared over-refusal next to
it: a file sourced with operands that shifts or sets its list. Each varied the list (a different
list refused, the same list allowed), and none put a dangerous value on the allowed side. The row
was caught only when the sentence claiming it was about to go into the pull request body, and was
run first with `$HOME/y` as the operand.

The fix compares by identity, not value: `shift` and `set --` each leave a new array, so a list
the file touched is unknown, and one it never touched restores the caller's. A planted by-value
comparison fails the guard's test (`78 pass, 1 fail`).

Rule: a claim that a row is correctly allowed is settled by running the dangerous payload on each
side of the boundary against the guard that makes the claim. Measuring where a rule's boundary
sits is not the same as running the dangerous row on each side of it. Write the claim down only
after that run.

Re-running everything is not a defence. More than ten rounds of re-derivation are what made three
reviewers confident enough to publish the same wrong row; the re-running was real, and the row was
in its output. More coverage would not have changed the outcome. An adversarial reading of what an
allowed row is evidence of would have.

## 2. A pattern count drops the name that looks like every other name and is not

Two extraction patterns, `[A-Za-z]+` and `[A-Za-z_]+:`, counted the guard's `State` interface at
17. One reviewer published that count; another caught its own 17 before publishing and diagnosed
the cause. The interface declares 18: both patterns stop at the digit in `argv0` and drop it. That round was about three sites that copy a child's `State` back into the
shell (`merge`, `runFunction`, and `runFile`'s sourced branch), each listing fields by hand, and
those lists had also left out the five fields that say where the walk is, `argv0` among them.

Prediction: an enumeration by pattern drops exactly the names the pattern does not expect, and a
list typed by hand drops the names nobody thinks of as fields. Derive the set from the declaration
itself, then check that each site's lists partition it. At #1536 each of the three sites' carried,
left-out and walk-position lists sums to 18: `merge` 9 + 4 + 5, `runFunction` 9 + 4 + 5, the
sourced branch 10 + 3 + 5. `argv0` crosses no site because no shell command writes it, only a new
`bash -c` child. That makes the omission checkable rather than asserted.

The guard now opens `merge()`'s comment with the rule: anything a copy-back site does not carry
out of a child is a candidate hole, and a field added to `State` belongs at all three sites, or in
each site's list of what it leaves out and why, or among the fields that cross no site.

## 3. Value equality stands for provenance only where the semantics are value

`merge`'s variable and array comparisons are by value, and there value equality is the semantics.
Two branches that leave the same value leave shells bash cannot tell apart. A follow-up pass ran
fifteen equal-by-value shapes, each through a branch, a source and a function: a variable re-set
to the same dangerous value, an array re-set to the same values, `cd` to where the shell already
is, a byte-identical function redefinition, a byte-identical EXIT handler re-set by a source, and
a substitution emitting one value twice. Every verdict matched bash.

Exactly two places in the guard compare where equal value does not mean the same thing, and both
compare by identity:

- The sourced-operands restore: the operand list and the caller's list can hold the same words
  while bash keeps one or the other.
- `handlerVars`: a handler lifted out of a branch reads the branch's value only while the shell
  still holds the exact array the lifting merge wrote. A later branch that blurs the name again
  writes the same placeholder text, so a content check would take the stale value.

The next one of these lives wherever two distinct objects can hold equal contents while bash
decides which of them survives. Look there before trusting a comparison by value.

---
title: "A record is true when written and read later: pin each claim to a command, or delete it"
category: legion
tags:
  - evidence
  - review-rounds
  - forward-merge
  - testing
  - pull-request-body
date: 2026-10-25
status: active
module: packages/daemon
related_issues:
  - "INFRA-83"
  - "sjawhar/legion#2121"
---

# A record is true when written and read later: pin each claim to a command, or delete it

Extends docs/solutions/legion/an-operator-facing-sentence-outlives-what-it-points-at-INFRA-31.md.

That document is about prose a person executes. The delta here is that the same failure runs
through **records a machine or a later role reads** — contract strings, run ids, counts,
fingerprints, test case lists — and through **the instruments built to catch it**. Fifteen
implementing rounds; every finding below was written correctly and read after its referent moved.

**The organising sentence: every instance is a value true when written and false where it was
read.** None was careless. The defence is re-derivation at the point of use, or a check that fires
when the referent moves.

**The operational test, before writing any claim:** *what would have to change for this to become
false, and would anything notice?* If nothing notices, the claim needs a re-derivation at the
point of use or a check that runs when the referent moves.

## The shapes, with their instances

| shape | instance in this change |
|---|---|
| A copied fact goes stale silently | the `skip-checks` rule written twice until the two disagreed; the hour's bound restated in six prose homes over one `time.Hour` constant |
| **A contract value is pinned in as many places as pin it, and each pin makes the wrong sentence look more confirmed** | `api/ready.go`'s already-merged note, false after this change, promoted into the published fixture, the golden, a test constant, three `tools.test.ts` strings and a contracts doc comment — six agreeing copies, none an independent witness |
| A search proves absence only of what it searched for | `grep -rln 'CI:'` silent about two live instructions that wrote the phrase without the colon |
| A rendered read proves nothing about what it did not show | a sentence reported absent from a document whose line the renderer had truncated at 768 bytes |
| A scope never contained the answer | a planner's list of fifteen short by four; a diff against the base that structurally cannot show what the base added |
| A measurement over a moving window cannot be checked | "the 40 most recent pull requests" names a different forty each hour |
| The absence of a signal is the failure nothing announces | the branch went conflicting **four times** with no event, no check and no wake; each was found by someone reading `mergeable` on a wake about something else, once after nineteen hours |
| **A check whose coverage was established by reading rather than by making it fail** | `permanentRefusal` excluded only the *stalled* half of `ghautomerge.Lagging`; four rounds and three roles read the gate and stopped, because the covered half looks like the whole case |

## The rules that came out of them

- **Event, not state.** A run id naming *what someone ran and saw* is an event: historical, re-derivable, safe in a record. A run id naming *what the forge currently holds* is a state with a clock, in a record that has none. The deleted `CI:` line was the second kind. Testable prediction: the moment an `E2E` line stops saying *I ran this and saw that* and starts saying *the head is green*, it has become the thing this issue deleted.
- **A sentence that cannot be true at the moment it is read is removed, not re-measured.** The body's `Size` line named a figure for four rounds and was wrong at the end of three; `re-read at this head` cannot hold in a line written before the push that changes it. The figure went; the command stayed.
- **A copy is admissible when it sits next to a check that falsifies it.** The defect is an unpinned copy, or one whose owner can change without the copy noticing. INFRA-149's trailer sentence lives verbatim in five texts and `skills-guard.test.ts` counts it in each; that is fine. The hour's bound lives in six prose homes with nothing holding them together; that is not.
- **Enumerate from the source that defines the set — then ask which states of each member are reachable, and which your rig can produce.** Enumerating types from `ghautomerge` still missed a `Lagging` that never stalls, because `Since` is `pull.UpdatedAt` read fresh every run. The move that found it: **read what the field is set from, not the state machine's names.**
- **Delete the term and re-run.** For the term you believe covers a case, delete it and re-run: either something turns red (it was load-bearing) or nothing does, and you have found an uncovered half. Cost here: one scratch edit and two runs, against a defect that had survived four rounds of reading — and a second application found a dead term in the fix itself.
- **A deny-list over an open set is wrong by default; an allow-list is silent by default.** Round 11 excluded `Lagging` from the `merge-waiting` gate and left `Disappeared` to fail the same way. The fix that held names what may spend the notice (`waitingOnGitHub`: `Armed`, `Unmergeable`, `Merging`), so a wait type added later is silent rather than wrong. The reviewer asked for the **shape** rather than the answer; that is the difference between the two rounds.
- **A fingerprint is an alarm, not a diagnosis.** When it moves, name the files whose diff moved and account for each. A changed fingerprint with three conflicts and five moved files leaves two unexplained, and an unexplained file is where a silent loss and a benign overlap look identical.
- **Say which instrument did not run, and why.** An unrun check reported as unrun costs nothing; an unrun check left unmentioned is the absence this whole document is about.
- **A body that grows a fixed amount per round has a round count at which the process fails.** Measured: 3,771 characters a round, against GitHub's 65,536 refusal — at which the body `PATCH` itself fails, so what breaks is the ability to update the record mid-round. Archiving round evidence into a pull-request comment moved it from 8.2 rounds to 10.4. Never link evidence into `.legion/<issue>/`: retro removes it at the merge, exactly when the record starts being read from outside the tree.

## Three instruments for three facts about a forward merge

Each is blind to the others' failure; none substitutes.

1. **Did the branch's diff change** — the unchanged-diff fingerprint. Blind to a restoration: when a merge keeps both sides, restored text leaves no line in the branch's diff.
2. **Are the branch's deletions still deleted** — the deletion-survival sweep over every removed line of ≥40 characters, each hit *read*. Blind to text the base added after the branch was cut.
3. **Did the base bring in text the branch forbids** — a standing lane that runs on every head. Blind to everything outside its patterns.

Scoping note with its asymmetry: dropping reflow-identical lines and lines in files the branch never touched is safe, because neither can hide a restoration. Dropping lines whose file the merge did not touch is **not** safe — one merge here touched 277 files while the branch's own diff was 76.

## The no-database rig, and its limit in the same sentence

`rememberWait` returns before it touches the pool when it has nothing to write and reaches the
pool when it does, so an outbox built on a `pgxpool` pointed at a dead port makes the gate report
its own decision: `nil` is a refusal, a dial failure naming the row is a write it set out to make.
Eleven causes driven that way, with the gate's clause reverted in a scratch copy to observe the
red-before. **It proves which way a gate decides, and never that a payload and its notice commit
in one transaction under the row's lease — that half needs a real Postgres.** A rig whose boundary
is unstated becomes the next person's over-claim.

Two cautions that cost something here: a fixture that bypasses the constructor measures a row the
system never makes (a hand-built `record.OutboxRow` left `Kind` empty, so `permanentRefusal` fell
to its default and reported a defect that does not exist); and a rig inherits the blind spot of
whoever enumerated its cases.

## Evidence

The change: `READY_CHECKS_NOT_GREEN` and the daemon's read of a head's check runs and commit
statuses are gone from `handoff_complete`; GitHub decides the merge under the base branch's own
rules; the pull request body's `CI:` line is deleted and each role reads CI on GitHub at the head
it judges; every deciding list read paginates; and a merge GitHub has not completed within an hour
of READY's acceptance tells the issue's architect once (`merge-waiting`, `auto_merge.go`).

Rounds that turned on a record rather than on code: the `Proven by` line stale in both halves at
once (a run id from round three, a head from round two) — found by two roles, each catching one
half, neither able to catch both, because a sentence falsifiable only by comparison with something
outside itself has no single reader; `required_checks.go:69`'s parenthetical, *"the reader READY
uses"*, true on `main` and false here, which no sweep could catch because the branch's retired
phrases are `READY_CHECKS_NOT_GREEN`, `no check is required on` and `refused at READY`; and a
`docs/solutions` census claimed in a proof's `surface` whose `command` never read that root —
running it turned up a file nobody had named.

**The instance this change left behind, recorded rather than quietly fixed:** the definition of
the tester's bare gates lives in two homes. `prompts/roles/core/tester.md:13` was corrected to
*"CI green at the new head, read on GitHub and written down nowhere"*;
`skills/legion-worker/references/pr-body.md:131` still reads *"the repository's CI green at the
new head"* with no such clause, in the sentence that tells the tester what to write into its `E2E`
line. The change about copies left a copy. It is reported to the architect rather than fixed here,
because retro may commit only `docs/solutions/` and the handoff removal, and a correction above
the approved head voids the approval — which is itself the lesson: **leave a round's width for the
findings a retro makes, because retro cannot take them.**

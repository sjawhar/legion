---
title: "A number or a guarantee restated in prose is a copy that drifts — cite the symbol, generate the table, and keep comments to what the code gives"
category: best-practices
tags:
  - documentation
  - code-comments
  - review-process
  - drift
  - generated-docs
date: 2026-10-25
status: active
module: packages/envoy
related_issues:
  - "INFRA-236"
  - "sjawhar/legion#2162"
applies_when:
  - "a constant's value, a limit or a guarantee is written in more than one place"
  - "a change adds comments faster than it adds code"
  - "a reviewer has asked twice for the same class of correction"
---

# A number or a guarantee restated in prose is a copy that drifts

- **Cite the symbol; do not restate the value.** Every copy of a number is a copy that the next
  tuning pass has to find. A constant gets one canonical comment, next to the code; docs and test
  comments name the symbol and say what it is for.
- **Generate the operator tables.** Alarm tables and limit tables are derived data. Where a
  generator already exists for the config and error pages, extend it rather than hand-maintaining
  a third table of the same facts.
- **Do not write a count, or the word "only", about a set a later change can grow.** Both were
  true when written and both went stale in this change — "the only signal that the fan-out is
  dead" survived the round that added three more such signals.
- **Hold a comment to what the code gives.** A comment that describes correct code wrongly passes
  every test, every lane, and every review that reads a comment as documentation rather than as a
  claim. That is why the class survives: nothing fails. Read each comment in a diff against the
  lines above it, and read the standard library or the other store's documentation rather than
  recalling it.
- **A comment that describes something outside its own file is an undeclared dependency.** It is
  correct when written and rots when the other thing changes, with no check anywhere. Cite the
  issue or the symbol; do not summarise what it contains.
- **Rulings, rejected alternatives and review history do not belong in code comments.** They are
  surface that drifts and that a future reader cannot verify. Put them in the pull request or in a
  document like this one; let the comment state what the code guarantees.
- **Weaker and true beats stronger and wrong, especially where the text licenses an irreversible
  act.** A procedure that tells a reader how to avoid needing a gate is worth more than one that
  makes the gate marginally more honest. When fixing a finding in such a procedure, add nothing
  you were not asked for: each new sentence is new surface for the next round.
- **When a sweep by hand does not converge, stop sweeping.** Four sweeps each finding more is a
  volume story, not a carelessness story: a change whose comments outweigh its code has more
  surface than any one sweep covers. A rule a careful person applies and still misses cases needs
  a smaller surface or a mechanical check, not more diligence.

## Evidence

INFRA-236 / sjawhar/legion#2162, which changed roughly as much prose as code.

- The broker-wide reread burst moved through five values on the branch. Each move left copies
  behind: round 1 caught two, round 2 four more, round 3 one more. The reread sizing derivation is
  restated in nine places across `limits.go`, two test files, `AGENTS.md`, three documentation
  pages and the pull request body.
- Seven comment-against-code defects were found across four sweeps. Four were found by the
  reviewer **after** the implementer had written the rule down and applied it; one was found by
  the implementer inside its own correction of another, with nobody checking — the claim that
  `http.Server.Shutdown` closes a still-reading connection rather than waiting, where
  `net/http`'s `closeIdleConns` treats a `StateNew` connection as idle only after five seconds
  without headers. That one is the strongest evidence that the check has to be mechanical: the
  rule was adopted, applied, and still missed.
- The operator procedure for converting a stored secret into a kind took three rounds. Round 3's
  fix added a remedy nobody had asked for ("run the retag again"), and that sentence was round 4's
  only blocking finding, because a peer that reads stale tags successfully logs nothing. The text
  that ended it recommends not converting at all.
- One comment in the final change cites a Dispatch issue by its contents ("the two ways to close
  it"); the issue's second version changed exactly that. The durable form is the bare reference.

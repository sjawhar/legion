# Replay classification: `skills/legion-architect/SKILL.md`

Source and method as for the root AGENTS.md file. For each merge the main-side commits that touched the skill between base and theirs were listed (`git log --first-parent base..theirs -- skills/legion-architect/SKILL.md`): 7 of the 16 merges meet `af460a46bb` (#2197, "rewrite Legion's own skills to the minimum") and 4 meet `56ba79b8cf` (#2192, "move the architect skill's Wake routing table to a reference").

Merges: 16; layout-caused: 1; not layout-caused (at least one SAME-SENTENCE hunk): 15.

Hunks by cause: ADJACENT 1, SAME-SENTENCE 20.

SAME-SENTENCE hunks by kind (clash = same words or fact; clauses = different clauses of one long sentence; enum = different items of one hand-written enumeration; rewrite = main rewrote or moved the text the branch edited; dup = both sides carry two versions of the same new text): clash 1, clauses 2, dup 2, rewrite 15.

Line numbers are base (merge-base) line numbers of that merge. `[auto]` marks a hunk labelled from the script's sentence diff without a manual read (only in merges already decided by another hunk).

## Every merge

### 22817ba1d2a3 — PR #2059 (legion/LEGION-781, 2026-10-20) — not layout-caused

base `a5b302e42403`, ours (branch parent) `0ec47d562429`, theirs (main parent) `3442ab023b9f`

- hunk 1: **SAME-SENTENCE** — base L244-248 (5 lines, longest 93 chars). [clauses] L244-248 integration-check sentence: ours "sign off the root" -> "accept the root (section 6)", theirs "current `main`" -> "the repository's default branch…"

### 458396560894 — PR #2136, #2160 (infra-140-s2, 2026-10-24) — LAYOUT-CAUSED

base `6fcac818ec32`, ours (branch parent) `eb397b7d91e7`, theirs (main parent) `92dbeea3d391`

- hunk 1: **ADJACENT** — base L38-40 (3 lines, longest 87 chars). ours appends the bullet "A finding outside your tree is not yours to file. …" after the "Deferring necessary work is failure." bullet (L38-40), whose second and third sentences theirs rewrites; no blank line between bullets

### 05caa7425c19 — PR #2188 (legion/INFRA-225, 2026-10-25) — not layout-caused

base `7bf2759c6e17`, ours (branch parent) `75f638135f21`, theirs (main parent) `56ba79b8cf6d`

- hunk 1: **SAME-SENTENCE** — base L265-281 (17 lines, longest 100 chars). [clash] L265-281 hand-over sentence: both rewrite its end "(`todo` with the `legion` label)."
- hunk 2: **SAME-SENTENCE** — base L431-457 (27 lines, longest 6038 chars). [rewrite] #2192 moved the Wake routing table (L431-457) to a reference; ours edited row L441

### 3c518b2dd970 — PR #2121 (legion/INFRA-83, 2026-10-25) — not layout-caused

base `f9b58bcdfbbb`, ours (branch parent) `96933b6e1762`, theirs (main parent) `14571f6f4aea`

- hunk 1: **SAME-SENTENCE** — base L317-319 (3 lines, longest 99 chars). [clauses] L317: ours adds `--paginate` to the `gh api` call, theirs drops the trailing "and, for a parent, that every child is `done` (section 3)" clause

### 5d8e06267f57 — PR #2121 (legion/INFRA-83, 2026-10-25) — not layout-caused

base `14571f6f4aea`, ours (branch parent) `05cd4b22e34c`, theirs (main parent) `56ba79b8cf6d`

- hunk 1: **SAME-SENTENCE** — base L431-457 (27 lines, longest 6038 chars). [rewrite] #2192 moved the Wake routing table; ours edited the merge-refused row L451-453

### 9c42d3cab8d8 — PR #2163 (legion/INFRA-251, 2026-10-25) — not layout-caused

base `b7ce539b6ecf`, ours (branch parent) `ea178a1d4c60`, theirs (main parent) `56ba79b8cf6d`

- hunk 1: **SAME-SENTENCE** — base L431-457 (27 lines, longest 6038 chars). [rewrite] #2192 moved the Wake routing table; ours edited L446

### ceec9f0dc5df — PR #2136, #2160 (infra-140-s2, 2026-10-25) — not layout-caused

base `6e66210b7732`, ours (branch parent) `e40f3083875f`, theirs (main parent) `d7026876b8c8`

- hunk 1: **SAME-SENTENCE** — base L431-457 (27 lines, longest 6038 chars). [rewrite] #2192 (with #2121, #2178) rewrote L431-457; ours rewrote the same lines

### 0ba7c5a321bf — PR #2136, #2160 (infra-140-s2, 2026-10-26) — not layout-caused

base `135d22e0f63b`, ours (branch parent) `31ad930be44c`, theirs (main parent) `af460a46bb16`

- hunk 1: **SAME-SENTENCE** — base L15-41 (27 lines, longest 99 chars). [rewrite] #2197 replaced L15-41 with "## Every turn"; ours inserted a bullet at L29 inside it
- hunk 2: **SAME-SENTENCE** — base L51-229 (179 lines, longest 130 chars). [rewrite] #2197 rewrote L51-229; ours edited lines 93-188 of it

### 35fb672ceca2 — PR #2160 (infra-140-s6, 2026-10-26) — not layout-caused

base `8298fc710323`, ours (branch parent) `b1462c558d53`, theirs (main parent) `ea460e50f257`

- hunk 1: **SAME-SENTENCE** — base L18-18 (1 lines, longest 170 chars). [dup] both add the "Before you write a spec or ask a person anything…" bullet after L18 (two versions); ours also extends L18

### 5f5b6c18fb6f — PR #2209 (legion/INFRA-391, 2026-10-26) — not layout-caused

base `fc379b99147a`, ours (branch parent) `08a478fdb44c`, theirs (main parent) `af460a46bb16`

- hunk 1: **SAME-SENTENCE** — base L438-457 (20 lines, longest 1027 chars). [rewrite] #2197 rewrote L438-457; ours edited L457

### 6c58a2716d7c — PR #2226 (legion/INFRA-252, 2026-10-26) — not layout-caused

base `81a3b5ef7b47`, ours (branch parent) `cb1483a8e11d`, theirs (main parent) `002a4ee98749`

- hunk 1: **SAME-SENTENCE** — base L233-249 (17 lines, longest 97 chars). [rewrite] #2197 turned the release paragraph L233-249 into bullets; ours appended sentences to it

### 8867bd5d67a1 — PR #2173 (legion/INFRA-80, 2026-10-26) — not layout-caused

base `135d22e0f63b`, ours (branch parent) `3b90e2498e8e`, theirs (main parent) `af460a46bb16`

- hunk 1: **SAME-SENTENCE** — base L411-428 (18 lines, longest 98 chars). [rewrite] #2197 rewrote L411-428; ours edited L417-421

### dadb5f515a3e — PR #2234 (legion/INFRA-448, 2026-10-26) — not layout-caused

base `135d22e0f63b`, ours (branch parent) `63d4f066ff01`, theirs (main parent) `8298fc710323`

- hunk 1: **SAME-SENTENCE** — base L411-428 (18 lines, longest 98 chars). [rewrite] #2197 rewrote L411-428; ours edited L425-428

### e2c36e932c9e — PR #2188 (legion/INFRA-225, 2026-10-26) — not layout-caused

base `135d22e0f63b`, ours (branch parent) `1a3d0b9ec2ee`, theirs (main parent) `c8974e2eb6aa`

- hunk 1: **SAME-SENTENCE** — base L51-229 (179 lines, longest 130 chars). [rewrite] #2197 rewrote L51-229; ours edited L61-64
- hunk 2: **SAME-SENTENCE** — base L233-249 (17 lines, longest 97 chars). [rewrite] #2197 rewrote L233-249; ours edited L239-249
- hunk 3: **SAME-SENTENCE** — base L253-281 (29 lines, longest 104 chars). [rewrite] #2197 condensed L253-281; ours inserted a sentence at L279

### edd6044b0113 — PR #2163 (legion/INFRA-251, 2026-10-26) — not layout-caused

base `56ba79b8cf6d`, ours (branch parent) `a7bf0f7fa522`, theirs (main parent) `63cd684a8256`

- hunk 1: **SAME-SENTENCE** — base L51-228 (178 lines, longest 130 chars). [rewrite] #2197 rewrote L51-228; ours edited L161-175
- hunk 2: **SAME-SENTENCE** — base L437-456 (20 lines, longest 1027 chars). [rewrite] #2197 rewrote L437-456; ours edited L449-452

### fae0cbd28b67 — PR #2280, #2319 (sami/gate-waits, 2026-10-27) — not layout-caused

base `ba9a24e238c3`, ours (branch parent) `b536ea79f95f`, theirs (main parent) `bc98d3e729b9`

- hunk 1: **SAME-SENTENCE** — base L16-16 (1 lines, longest 209 chars). [dup] both extend bullet L16 with two versions of "Open an ordinary ask on the root issue itself, never a child: …"

## Proposed replay pairs

Only one merge (`458396560894`) is layout-caused; two more (`22817ba1d2a3`, `3c518b2dd970`) are different clauses of one sentence. The other thirteen are content clashes: eleven meet a main-side rewrite or move of the skill (seven #2197, which rewrote the skill to the minimum; four #2192, which moved the Wake routing table to a reference) and two carry two versions of the same new text (`35fb672ceca2`, `fae0cbd28b67`). Where the branch side's edits landed: nine merges in prose hard-wrapped at 130 columns or less; five in rows of the Wake routing table (base L431-457; the rows edited are 245 to 1,231 characters, the longest row 6,038), which #2192 moved out of the skill whole; and two in the `## Every turn` bullets L16 (209 characters) and L18 (170). In every SAME-SENTENCE merge the main side rewrote, moved or re-added the same block (the main side touches 1 to 179 lines of the hunk), so the length of the edited line is not what made it conflict. Restructuring this skill for layout is not warranted by this evidence: semantic line breaks would have prevented one conflict in sixteen. (At `main` today, after #2197, the skill is 85 lines with 41 lines over 100 characters, up to 1,137; that layout is new and not what produced these conflicts.) Six layout pairs do not exist for this file; the pairs below are the one layout merge and two controls.

### ADJACENT (bullet appended after an edited bullet): 458396560894 — PR #2136, #2160

Ours appends a bullet after the "Deferring necessary work is failure." bullet, whose second and third sentences theirs rewrites. Expected: clean with a blank line between bullets.

- merge `458396560894`; base `6fcac818ec32`; ours `eb397b7d91e7`; theirs `92dbeea3d391`; hunk causes: ADJACENT
- hunk 1 (base L38-40):
  - ours: insert after base L40 (ending «nuinely independent child through the controller rather than treating it as an abandoned dependency.»): «- A finding outside your tree is not yours to file. Put it in your own record if it bears on your work. Otherwise, send it to the controller with what dropping it would cost, and the controller puts it to the person as a question on the issue that surfaced it.»
  - theirs: base L38-40: «The sole valid deferral is a new child issue you create and continue to own.» -> «The sole valid deferral is a new child issue you create and continue to own, or, on a root's split, a `dispatch ask` for the rest.»; all changes: ` … create and continue to [-own.-] {+own, or, on a root's split, a `dispatch ask` for the rest.+}`
  - theirs: base L38-40: «Re-file a genuinely independent child through the controller rather than treating it as an abandoned dependency.» -> «A child that looks independent goes to the controller (*Escalation judgment*); a problem outside the tree is a `dispatch ask`, never an issue you file (`skill://legion-worker/references/findings.md`).»; all changes: `[-Re-file a genuinely-] {+A child that looks+} independent [-child through-] {+goes to+} the controller [-rather than treating it as-] {+(*Escalation judgment*); a problem outside the tree is a `dispatch ask`, never+} an [-abandoned dependency.-] {+issue you file (`skill://legion-worker/references/findings.md`).+}`

### CONTROL (SAME-SENTENCE): 22817ba1d2a3 — PR #2059

Integration-check sentence L244-248: ours "sign off the root" -> "accept the root (section 6)", theirs "current `main`" -> "the repository's default branch your addressing line names". Expected: conflict (one sentence).

- merge `22817ba1d2a3`; base `a5b302e42403`; ours `0ec47d562429`; theirs `3442ab023b9f`; hunk causes: SAME-SENTENCE
- hunk 1 (base L244-248):
  - ours: base L244-248: «tree, the root included, so the root's tester can run, and its pull request merge, before any child merges.» -> «tree, the root included, so the root's tester and reviewer can run, and the root reach `accepting`, before any child merges; your acceptance is where the root waits for its children (section 7).»; all changes: `tree, the root included, so the root's tester {+and reviewer+} can run, and [-its pull request merge,-] {+the root reach `accepting`,+} before any child [-merges.-] {+merges; your acceptance is where the root waits for its children (section 7).+}`
  - ours: base L244-248: «…nt `main`, release it with `release_children` only once every other child has closed, and sign off the root only after it closes.» -> «…nt `main`, release it with `release_children` only once every other child has closed, and accept the root (section 6) only after it closes.»; all changes: ` … child has closed, and [-sign off-] {+accept+} the root {+(section 6)+} only after it closes.`
  - theirs: base L245-248: «To get parent integration evidence against current `main` after the last child merges, file the parent's integration check as a final child whose acceptance is every parent criterion proven on current `main`, release it with `release_children` only once every other child h…» -> «To get parent integration evidence against the repository's default branch your addressing line names after the last child merges, file the parent's integration check as a final child whose acceptance is every parent criterion proven on that default branch, release it with…»; all changes: `To get parent integration evidence against [-current `main`-] {+the repository's default branch your addressing line names+} after the last child … parent criterion proven on [-current `main`,-] {+that default branch,+} release it with `release_children` … `

### CONTROL (SAME-SENTENCE, rewrite): 9c42d3cab8d8 — PR #2163

Main moved the Wake routing table to a reference; ours edited a row of it (L446). Expected: conflict.

- merge `9c42d3cab8d8`; base `b7ce539b6ecf`; ours `ea178a1d4c60`; theirs `56ba79b8cf6d`; hunk causes: SAME-SENTENCE
- hunk 1 (base L431-457):
  - ours: base L446: «Its `version` and `reason`. A human asked for changes to the root spec at `version`, for `reason`. Revise the spec as the reason asks and request approval again as section 1 says; stay parked; the gate is closed.» -> «Its `version` and `reason`: a human asked for changes to the root spec at `version`. Revise, request approval and `register_gate` again as section 1 says, then end the turn; the gate is closed.»; all changes: `Its `version` and [-`reason`. A-] {+`reason`: a+} human asked for changes … the root spec at [-`version`, for `reason`. Revise the spec as the reason asks and-] {+`version`. Revise,+} request approval {+and `register_gate`+} again as section 1 [-says; stay parked;-] {+says, then end the turn;+} the gate is closed.`
  - theirs: base L431-457: «Handle one delivered wake by verifying the relevant live artifact and then performing the corresponding lifecycle procedure. Architect-addressed wakes always reach the architect that owns the payload issue: a claimed child sub-architect,…» -> «Handle every delivered wake from the table in `skill://legion-architect/references/wake-routing.md`: read it when a wake arrives, and follow the row for that wake.»; all changes: `Handle [-one-] {+every+} delivered wake [-by verifying the relevant live artifact and then performing the corresponding lifecycle procedure. Architect-addressed wakes always reach the architect that owns the payload issue: a claimed child sub-architect, otherwise the nearest claimed ancestor, then the root. A wake about an architect role's own launch reaches the architect above it. `phase-finished`, worker lifecycle, child lifecycle, and design-gate wakes are architect-only and never go to an active phase worker. Wake Procedure --- --- `carried-gate-opened` Only a root whose catch-up said `car`


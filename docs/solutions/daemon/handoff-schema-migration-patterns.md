---
title: "Handoff schema migration and cross-cutting field patterns"
category: daemon
tags:
  - handoff
  - zod
  - schema-migration
  - backward-compatibility
  - discriminated-union
date: 2026-04-05
status: active
module: daemon
related_issues:
  - "#239"
  - "#218"
  - "LEGION-53"
  - "sjawhar/legion#1028"
  - "LEGION-131"
  - "sjawhar/legion#1106"
symptoms:
  - "z.discriminatedUnion requires ZodObject not ZodEffects"
  - "how to add fields to all handoff phases"
  - "how to rename a handoff field with backward compatibility"
  - "how to make one handoff field required only when another is absent (cross-field rule)"
  - "legion handoff write: Invalid implement handoff: proof: Invalid input: expected array, received undefined"
  - "legion handoff write: Invalid test handoff: failures: a rejected implementer proof is a recorded failure"
  - "legion handoff read: [handoff] Ignoring <file>: phase: expected one of architect|plan|implement|test|review"
---

# Handoff Schema Migration and Cross-Cutting Field Patterns

## Overview

The handoff system uses a `BaseHandoff` interface/schema that all phase-specific types extend. When adding cross-cutting fields or renaming existing ones, the Zod discriminated union imposes constraints on where migration logic can live.

## Adding Cross-Cutting Fields to All Phases

Adding a field to `BaseHandoff` propagates to all phases automatically:

1. **TypeScript interface** (`types.ts`): Add to `BaseHandoff` — all phase interfaces inherit via `extends`
2. **Zod schema** (`schema.ts`): Add to `baseHandoffSchema` — all phase schemas use `.extend()`
3. **No per-phase changes needed** for the schema layer

This works because each phase schema is defined as `baseHandoffSchema.extend({ phase: z.literal("..."), ...phaseSpecificFields })`.

## Renaming Fields with Backward Compatibility

When renaming a field (e.g., `learningsUsed` → `learningsInjected`), the Zod discriminated union constrains your approach:

**The constraint (Zod 3 only — see the next section for Zod 4):** `z.discriminatedUnion("phase", [...])` requires all members to be `ZodObject` types. On Zod 3, adding `.transform()` to a member converts it to `ZodEffects`, which the discriminated union rejects at compile time.

**The solution:**

1. **Keep the old field in the Zod schema** with `@deprecated` JSDoc and `.optional()` — old JSON files still parse
2. **Remove the old field from the TypeScript interface** — prevents new code from writing it
3. **Add migration logic in `validatePhaseHandoff()`** — the wrapper function that calls `safeParse()`:

```typescript
if (data.phase === "plan" && "learningsUsed" in data) {
  const { learningsUsed, ...rest } = data as Record<string, unknown>;
  if (Array.isArray(learningsUsed) && !rest.learningsInjected) {
    (rest as Record<string, unknown>).learningsInjected = learningsUsed;
  }
  return rest as unknown as PhaseHandoff;
}
```

4. **Precedence rule**: New field wins if both exist (safe for mixed-state files on branches)

**The type cast chain** (`as Record<string, unknown>` → `as unknown as PhaseHandoff`) is necessary because Zod's inferred type still includes the deprecated field from the schema, while the TypeScript interface doesn't. This is a known friction point when schema and interface diverge intentionally.

## Cross-field rules on one phase (Zod 4): `.refine()` stays a `ZodObject`

LEGION-53 (`sjawhar/legion#1028`) needed "a test handoff must carry the tester's own `proof`
unless it reports a failure" — a rule across three fields of `testSchema`. The section above
would have sent that into `validatePhaseHandoff()`. It does not have to go there any more: the
repository is on Zod 4 (4.3.6 in the workspace lockfile), and on Zod 4 `.refine()` returns the
`ZodObject` itself — `refined.constructor.name === "ZodObject"`, `_zod.def.type === "object"` —
not a wrapper. Verified at runtime in `packages/contracts` with `bun -e`:

- `z.discriminatedUnion("phase", [testSchema.refine(...), ...])` builds and routes `phase: "test"`
  to the refined member; the refinement's issue comes back with the `path` you gave it
  (`proof: a passing test handoff needs the tester's own production-like proof`).
- `.passthrough()` and `.refine()` compose in either order: `refine().passthrough()` keeps the
  refinement and the unknown keys, and so does `passthrough().refine()`. The `.passthrough()`
  that used to sit at the union site (`implementSchema.passthrough()` inside the array) now sits
  on each phase schema's own definition and the union takes the five schemas directly — a
  readability choice, not a constraint.

The rule that decides whether a handoff is "passing" is written once, on the schema, as data:

```typescript
.refine(
  (handoff) =>
    (handoff.failures?.length ?? 0) > 0 ||
    (handoff.failed ?? 0) > 0 ||
    (handoff.proof?.length ?? 0) > 0,
  { path: ["proof"], message: "a passing test handoff needs the tester's own production-like proof" }
);
```

Two things follow for the ledger. **Name the field in every refusal:** `describePhaseHandoffProblems()`
(`handoff-schema.ts`) maps each Zod issue to `<path joined by .>: <message>` (`<root>` when the
path is empty), and both the write refusal (`Invalid <phase> handoff: proof: …`, thrown before
`ensureLegionDir`, so nothing is created) and the read warning (`[handoff] Ignoring <file>: …`
on stderr before the existing `return null`) print that same list — one formatter, two surfaces.
**Every fixture that writes the stricter phase changes in the same commit:** `ledger.test.ts`,
`cli/__tests__/handoff.test.ts`, and the two `knowledge/__tests__` files all wrote proof-less
implement handoffs; two of the ledger fixtures existed to isolate a *different* invalid field
(`trickyParts: "not an array"`, `completed: "Tuesday"`), so each had to gain a valid `proof` or
its `null` would stop proving what the test name says.

What did **not** change: `HANDOFF_SCHEMA_VERSION` stays 1 (a pre-change `implement.json` on an
in-flight branch simply fails validation and reads as missing, with the stderr line saying which
field), and the daemon never reads a handoff, so the enforcement point is the CLI every
implementer runs.

**Inventory every reader before tightening a phase schema, not only the write/read pair you edit.**
`readPhaseHandoff`/`readAllHandoffs` have three callers: `cli/index.ts` (`legion handoff read`),
`knowledge/collector.ts` (`collectLearningFeedback`), and `knowledge/feedback-logger.ts`. The last two
are what `legion knowledge consolidate` runs, and a handoff that now reads as `null` silently drops
its `learningsInjected`/`learningsHelpful` from the consolidation. LEGION-53's Deployment section
named the first consequence (the stderr line) and missed the second until the reviewer added it;
the operator note became "run the consolidate once on the old build before restarting the daemon
on the merged `main` if that feedback is wanted". `grep -rn 'readPhaseHandoff\|readAllHandoffs'
packages/daemon/src` and write one sentence per caller about what it does with `null`.

## Tightening the same schema a second time (LEGION-131, `sjawhar/legion#1106`), and what it left for the next toucher

LEGION-53's reviewer named the holes its own change left, and LEGION-131 closed them in
`handoff-schema.ts` without a version bump: `nonEmpty = z.string().trim().min(1)` on every `Proof`
field and `implementerProof.how` (whitespace is not a proof); two more `.refine()` clauses on
`testSchema`, each with `path: ["failures"]` — `failed > 0` needs a recorded failure, and so does
`implementerProof.verdict === "rejected"` (the prose in the worker skill already said so; the machine
check now agrees); the one `.passthrough()` hoisted onto `baseHandoffSchema` (`.extend()` and
`.refine()` both preserve the catchall, and the discriminated union accepts the result — pinned by a
test that parses `{undeclared: "kept"}` at all five phases and fails when the base catchall is
removed); and `describePhaseHandoffProblems` rendering the union's unmatched discriminator —
`issue.code === "invalid_union"` at path `["phase"]`, the shape zod 4.3.6 emits for an unknown string,
a missing key, and a non-string alike — as `phase: expected one of ${HANDOFF_PHASES.join("|")}` instead
of the bare `Invalid input`. Proven on the branch CLI in scratch workspaces before and after
(baseline: a blank `observed` wrote, `failed: 1` with no failures wrote, rejected-with-clean-pass wrote,
`phase: "retro"` read as `phase: Invalid input`); the tester's live contrast with the pane's deployed
`legion`, which still accepted all of it, is in
[`../legion/worker-pane-shell-gotchas.md`](../legion/worker-pane-shell-gotchas.md) §13.

Residuals a fresh-eyes review of the diff named; none blocks, each is the first thing to check when
this schema is touched again:

- **`.trim()` is a transform.** `writePhaseHandoff` persists the raw payload; `readPhaseHandoff` returns
  the parsed value, so `legion handoff read` prints `exit 1` for a file that holds `"  exit 1  "` (the
  tester saw it). Nothing compares the two today; a checksum-vs-read comparison would disagree.
  `z.string().refine((s) => s.trim().length > 0)` keeps the round trip lossless if that ever matters.
- **The rejected-verdict refine dereferences `handoff.implementerProof.verdict`.** Safe only because
  `implementerProof` is required and zod 4 skips refinements after an aborting property issue; make
  `implementerProof` optional later and a refusal becomes a `TypeError`. `?.verdict` costs nothing.
- **`failed` is `z.number()`.** `failed: -1` is refused by a message that says `failed > 0`; `1.5` is
  admitted. `z.number().int().min(0)` states the contract.
- **The `invalid_union` heuristic swallows any future `invalid_union` at `phase`**, whatever its cause;
  the test pins zod 4.3.6's shape, so a zod bump that changes it fails loudly — keep that test.
- **Two prose drifts the skill text now carries.** `skills/legion-retro/SKILL.md` step 1 still lists
  the Legion-specific example surface (`packages/daemon/src/daemon/__tests__/`, "a live check at the
  operator's next daemon restart") that the worker skill's single proof definition replaced with the
  generic "the repository's real-process test harness and fixtures"; and that definition says each
  `E2E` line "carries a link", which a CLI-in-a-scratch-workspace proof cannot — the exact command and
  its output is the record there. Both are one-sentence edits for whoever next touches those files
  (`../legion/text-only-skill-pr-mechanics.md` §5: skill and prompt are two copies of one rule).
- **Say "`packages/daemon/src/daemon/` is untouched", not "no daemon behaviour change".** The CLI's
  `handoff write`/`read` behaviour is exactly what a contracts tightening changes, through the import;
  the reviewer's phrasing is the precise one.

## Testing Schema Migrations

Test the 4-quadrant matrix for any field rename:

| Scenario | What to verify |
|----------|---------------|
| Old field only | Migration maps to new field |
| New field only | New field preserved as-is |
| Both fields present | New field takes precedence |
| Neither field | Both undefined, no regression |

Two more rules for the daemon's persisted-state migrations (`legion-state.ts`), from LEGION-20:

- **The next version number is a shared resource, re-checked against `main` at every rebase.**
  Two branches both claimed v25, then two claimed v26, then two claimed v27; the later one renumbered (to v28 in the end) inside the commit that
  introduced the number. See
  `docs/solutions/legion/schema-bump-branch-rechecks-mains-version-at-every-rebase.md`.
- **A migration that reads an external system runs exactly once.** `loadState` writes the
  migrated state right after the `.bak`, so a restart before the first ordinary save does not
  repeat the reads (or re-depend on the external system at boot); the test reloads the file with
  a resolver that throws.

## Updating Workflow Markdown Files

When a handoff field change affects workflow examples:

- Search for `legion handoff write --phase` across all 5 workflow files to find every handoff write block
- **`implement.md` has TWO handoff write sections** (fresh implementation + address-comments mode) — both need updating
- Check all workflow files for cross-references to the old field name (e.g., `review.md` referenced `plan.learningsUsed` in a different section than its own handoff write)
- Each workflow's handoff section follows a consistent structure: `[assessment prose] → [bash block] → [key fields list]`

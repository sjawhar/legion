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
symptoms:
  - "z.discriminatedUnion requires ZodObject not ZodEffects"
  - "how to add fields to all handoff phases"
  - "how to rename a handoff field with backward compatibility"
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

**The constraint:** `z.discriminatedUnion("phase", [...])` requires all members to be `ZodObject` types. Adding `.transform()` to a member converts it to `ZodEffects`, which the discriminated union rejects at compile time.

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

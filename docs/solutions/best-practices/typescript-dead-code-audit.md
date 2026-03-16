---
title: "TypeScript Dead Code Audit: Patterns and Pitfalls"
category: best-practices
tags:
  - dead-code
  - typescript
  - cleanup
  - noUnusedLocals
  - barrel-exports
  - zod
  - refactoring
date: 2026-03-16
status: active
related_issues:
  - "OKM-40"
symptoms:
  - "noUnusedLocals errors after adding tsconfig flag"
  - "unused exports accumulating over time"
  - "orphaned files that still compile"
---

# TypeScript Dead Code Audit: Patterns and Pitfalls

**Context:** OKM-40 removed 480 lines of dead code across `packages/daemon` and `packages/opencode-plugin` — orphaned files, unused exports, stale type fields, and undeclared-but-unread tool arguments. This doc captures patterns for future audits.

## Dead Code Accumulation Patterns

Four distinct mechanisms produce dead code in this codebase:

**1. Feature supersession without cleanup**
`state/cli.ts` even had a doc comment saying "Superseded by POST /state/collect on the daemon" — the code documented its own obsolescence. Files don't get deleted because the new path works; there's no broken import forcing cleanup.

**2. Optimistic exports (export-first, no consumer ever materializes)**
Types like `GitHubLabel`, `GitHubPR`, `WorkerState`, `IssueStateDict`, `CollectedStateDict` were exported anticipating future consumers. Without a linter enforcing consumption, exports are free and accumulate. They mislead future readers into thinking something is used externally.

**3. Planned-but-abandoned fields**
`BackgroundTask` fields (`retryCount`, `concurrencyKey`, `lastMessageCount`, `lastActivityAt`, `staleAlertSent`) and `LaunchOptions.skills` were scaffolded during domain modeling but the features were never implemented. These accumulate during iterative development when you model the full domain upfront.

**4. Argument stubs**
`run_in_background` and `include_transcript` were declared as tool schema arguments but never read in the handler body. This happens when tool schemas are drafted based on desired UX before the implementation ships.

## Audit Methodology

### Static analysis is necessary but not sufficient

`noUnusedLocals` and `noUnusedParameters` in tsconfig catch locally unused variables and parameters but are **blind to module-level exports** — an exported symbol is never "unused" from the compiler's perspective within the file that declares it. The bulk of a dead code audit requires manual investigation:

```bash
# Find exports not imported anywhere
grep -r "export.*LegionEntry\|export.*WorkerState" packages/ --include="*.ts"
# Then verify no imports
grep -r "import.*LegionEntry\|import.*WorkerState" packages/ --include="*.ts"
```

### Check tests explicitly

Test files import things that production code doesn't. `isPidAlive` was nearly removed until grepping for `__tests__` found it was legitimately tested and exported. Always include test imports in your grep:

```bash
# Don't exclude __tests__ when verifying an export is unused
grep -r "import.*isPidAlive" packages/ --include="*.ts"
# (includes test files by default — that's what you want)
```

### Cross-package check in monorepos

A symbol unused in its own package may be imported by a sibling package. Check all packages before removing re-exports:

```bash
grep -r "from.*state/types" packages/ --include="*.ts"
```

### Check for dynamic consumers

Before deleting a file or symbol, search for string-based references that the TypeScript compiler won't see:

```bash
grep -r '"cli"' packages/ --include="*.ts" --include="*.json"
grep -r "'short-id'" packages/ --include="*.ts"
```

## Removing Fields from Zod `.passthrough()` Schemas

The `BackgroundTaskSchema` uses `.passthrough()`, which lets unknown keys flow through unchanged. This makes field removal **safe at runtime** but requires careful type-level verification:

**Safe to remove a field from a `.passthrough()` schema when:**
1. No code reads the field from the typed output (grep for all accesses)
2. You also remove it from the TypeScript interface/type
3. Either you no longer need the field, or you're removing it from the sender too

**Do NOT remove when:**
- The field is read after parsing (even in a rarely-executed branch)
- The schema is shared with an external API where you don't control the sender and need to maintain wire compatibility
- The field being present vs. absent changes downstream behavior in ways not visible via TypeScript

**Key distinction:** `.passthrough()` prevents *rejection* of extra fields, but it doesn't make the TypeScript type accept them. Once you remove a field from the interface, any access to it is a compile error — which is exactly the verification you want.

## Barrel File Hygiene (`index.ts`)

The `tools/task/index.ts` barrel was re-exporting 19 lines of symbols from internal modules. Only `createTaskTools` was consumed externally. The rest were "in case someone needs them."

**The problem:** Barrel files are written once at module creation and rarely pruned. Future developers see the barrel and assume all exports are intentional and consumed — they become afraid to remove them.

**The rule:** A barrel should export exactly what external consumers import. If you can't name a current consumer for an export, it doesn't belong in the barrel.

**Important:** `noUnusedLocals` does NOT catch barrel over-exports. The only reliable enforcement is either:
- An ESLint rule (`eslint-plugin-import` with `no-unused-modules`)
- Periodic manual audit (like this one)

Keep barrels minimal by default: start with only what's needed, add exports on demand rather than starting with everything and pruning later.

## Adding `noUnusedLocals` / `noUnusedParameters`

These flags prevent new dead code from accumulating but have tradeoffs:

**What they catch:** Local variables declared but never read; function parameters declared but never used.

**What they do NOT catch:** Exported symbols with no consumers (the bulk of a dead code audit).

**Tradeoffs:**
- Requires `_` prefix on intentionally-ignored parameters (e.g., `(_event, value) => ...`)
- Can break callback patterns unless prefixed
- Biome's `noUnusedVariables` rule catches the same class of issues and may produce duplicate warnings

**How to add without pain:**
1. Add the flags to a scratch tsconfig first to see what they catch
2. Fix all violations in a single commit that also adds the flags
3. Never commit the flag with outstanding violations suppressed by `// @ts-ignore`

**These flags are the finish line, not the starting gun.** The bulk of OKM-40's value (the 18-file export audit, field removals, orphan deletions) required manual investigation that these flags alone would never surface. Add them last, as regression prevention.

## Biome Complements TypeScript

After removing `export` from `WorkerState` and `TaskGetInput` (making them module-private), Biome's `noUnusedVariables` rule caught that these types were also unused *locally* — they should be deleted entirely, not just unexported. The combination of:

1. TypeScript compiler (`noUnusedLocals`) for locals/parameters
2. Biome (`noUnusedVariables`) for module-level unused symbols
3. Manual grep for exported symbols with no cross-module consumers

...covers all three levels of dead code.

## Checklist for Future Dead Code Audits

- [ ] Run baseline typecheck before any changes to confirm clean start
- [ ] Search for dynamic consumers (string-based references, `require()`, config files)
- [ ] Check all packages in the monorepo for cross-package imports
- [ ] Include `__tests__` directories in import searches
- [ ] For `.passthrough()` schema fields: grep for all read accesses before removing
- [ ] For barrel files: verify every export has a named external consumer
- [ ] After making changes: run Biome in addition to `tsc` — it catches newly-orphaned locals
- [ ] Add `noUnusedLocals`/`noUnusedParameters` last, after all cleanup is done
- [ ] Run full test suite to confirm nothing broke

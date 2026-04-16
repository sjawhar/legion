---
title: Per-writer file partitioning to eliminate merge conflicts in concurrent systems
date: 2026-04-16
category: architecture-patterns
tags:
  - merge-conflicts
  - concurrent-writes
  - file-partitioning
  - knowledge-index
  - retro
  - migration
module: daemon
related_issues:
  - "583"
  - "581"
symptoms:
  - "merge conflicts in shared JSON file when multiple workers write simultaneously"
  - "6.4% of worker sessions hit index.json merge conflicts"
  - "retro workers contending on same file on same branch"
status: active
---

# Per-Writer File Partitioning to Eliminate Merge Conflicts

## Problem

`docs/solutions/index.json` was a shared append-only file mapping path prefixes to learning doc paths. Multiple retro workers writing to this file simultaneously caused merge conflicts in 6.4% of sessions (76/1181 over 10 days). The file was 22.8KB with 140 entries. I/O performance was fine — the problem was structural contention.

## Root Cause

Identified via empirical analysis (#581): the conflict rate correlated with concurrent retro workers, not file size or I/O speed. Any shared mutable file written by multiple concurrent agents will eventually conflict, regardless of how fast the writes are.

## Solution: Per-Writer File Partitioning

Replace the single shared file with per-writer files in a directory:

```
# Before: single shared file (conflict-prone)
docs/solutions/index.json

# After: per-writer files (conflict-free)
docs/solutions/.index/_legacy.json    # migrated entries
docs/solutions/.index/LEG-583.json    # entries from issue 583
docs/solutions/.index/LEG-584.json    # entries from issue 584
```

Each worker writes only its own file. The full index is assembled at read time by scanning all files in the directory.

### Key Design Decisions

1. **Read path vs. write path separation**: The promoter reads the assembled index (scanning all files) to match against existing prefix keys, but writes only the new entries to a per-issue file. This prevents duplicate entries while avoiding write contention.

2. **`entries` key (not `index`)**: Per-entry files use `{ "entries": {...}, "version": 1 }` while the assembled view uses `{ "index": {...}, "version": 1 }`. This makes it impossible to accidentally treat an entry file as the full index or vice versa.

3. **`_legacy.json` for migration**: Existing entries lacked issue provenance data, so they couldn't be split by issue ID. A single `_legacy.json` file holds all migrated entries. This is a one-time migration artifact — new entries always go to per-issue files.

4. **Soft cap is advisory**: The ~10-entry-per-key soft cap is applied to the assembled view during promotion but individual entry files are not retroactively trimmed. This is acceptable because the cap exists to keep planner context manageable, not to enforce a hard limit.

## Implementation Pattern

```typescript
// Read: assemble from all files
export async function readAssembledIndex(indexDir: string): Promise<KnowledgeIndex> {
  const assembled: KnowledgeIndex = { index: {}, version: 1 };
  const files = await readdir(indexDir);
  for (const file of files.filter(f => f.endsWith(".json"))) {
    const entry = parseEntry(await readFile(path.join(indexDir, file), "utf-8"));
    // Merge entry.entries into assembled.index
    for (const [key, paths] of Object.entries(entry.entries)) {
      assembled.index[key] = [...new Set([...(assembled.index[key] ?? []), ...paths])];
    }
  }
  return assembled;
}

// Write: only this worker's entries
export async function applyPromotions(
  indexDir: string, docsRoot: string,
  promoted: PromotableLearning[], entryId: string
): Promise<PromotionResult> {
  const assembledIndex = await readAssembledIndex(indexDir);
  // ... compute new entries using assembled index for dedup ...
  const entryFile = path.join(indexDir, `${sanitizeEntryId(entryId)}.json`);
  await writeFile(entryFile, JSON.stringify({ entries: newEntries, version: 1 }));
}
```

## When to Apply This Pattern

Use per-writer file partitioning when:
- Multiple concurrent writers append to the same file
- Writers don't need to read each other's latest writes in real-time
- The data can be assembled at read time without loss
- Merge conflicts are the primary pain point (not I/O performance)

**Don't use this pattern when:**
- Writers need atomic read-modify-write semantics
- The file must be a single coherent document (e.g., a config file)
- There's only one writer (use the callback consolidation pattern from `shared-state-file-ownership.md` instead)

## Skill/Workflow Update Coordination

This change required updating 7 skill/workflow files to reference the new `.index/` directory pattern instead of `index.json`. When changing infrastructure that skills reference:

1. **Grep all skill files** for references to the old pattern
2. **Update instructions and examples** — skills are the interface between the system and agents
3. **Add explicit "do NOT" guards** — e.g., "Do NOT read or modify `docs/solutions/index.json`" prevents agents from reverting to the old pattern
4. **Deprecate, don't delete** — the old `index.json` was kept (with a `.backup` suffix) rather than deleted, allowing rollback if needed

## Related

- `shared-state-file-ownership.md` — the callback consolidation pattern for single-writer scenarios
- `task-index-retro.md` — earlier analysis of index-related patterns
- #581 — empirical analysis that identified the root cause

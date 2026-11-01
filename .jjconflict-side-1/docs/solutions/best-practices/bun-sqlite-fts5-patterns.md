---
title: "FTS5 Full-Text Search with bun:sqlite"
date: 2026-03-01
category: best-practices
tags: [bun, sqlite, fts5, full-text-search, content-store]
related-issues: [67]
related-prs: [75]
---

# FTS5 Full-Text Search with bun:sqlite

Patterns and gotchas from building an in-process FTS5 knowledge base for tool output compression (#67).

## Use `bun:sqlite` Over `better-sqlite3`

For plugins running inside Bun processes, `bun:sqlite` is the right choice:
- Zero external dependencies (no native addon compilation)
- API nearly identical to `better-sqlite3` (synchronous, statement-based)
- No version mismatch risk between Node native addon and Bun runtime

## Dual-Index Architecture

Two FTS5 virtual tables provide complementary search capabilities:

```sql
-- Porter stemming: "retries" matches "retry", fast, handles word forms
CREATE VIRTUAL TABLE porter_index USING fts5(
  source UNINDEXED, session UNINDEXED, title, content,
  tokenize='porter unicode61'
);

-- Trigram: "chronizat" matches "synchronization", substring matching
CREATE VIRTUAL TABLE trigram_index USING fts5(
  source UNINDEXED, session UNINDEXED, title, content,
  tokenize='trigram'
);
```

Three-layer search fallback: Porter → Trigram → Levenshtein fuzzy. Each layer is slower but catches more edge cases. Stop at the first layer that returns results.

**Trade-off**: Every insert goes to both tables, doubling write cost. Acceptable for the expected volume (tool outputs, not high-throughput writes).

## Key Gotchas

### WAL Sidecar File Cleanup

SQLite WAL mode creates `-wal` and `-shm` sidecar files alongside the main `.db`. If you only delete the `.db` on cleanup, the sidecars persist with indexed content in `/tmp`.

```typescript
close(): void {
  this.db.exec("PRAGMA wal_checkpoint(TRUNCATE)");
  this.db.close(false);
  for (const suffix of ["", "-wal", "-shm"]) {
    const p = this.dbPath + suffix;
    if (fs.existsSync(p)) fs.rmSync(p, { force: true });
  }
}
```

### FTS5 `bm25()` Scores Are Negative

`bm25()` returns negative values where **lower = better match**. Normalize before exposing to callers:

```typescript
private normalizeFtsScore(raw: number, weight: number): number {
  return weight / (1 + Math.abs(raw));
}
```

Use the `weight` parameter to distinguish search tiers (1.0 for Porter, 0.8 for Trigram, 0.6 for fuzzy).

### FTS5 Query Sanitization

FTS5 `MATCH` queries throw on special characters (`+()[]{}"`, etc.). Never pass raw user input directly. Split on non-alphanumeric, filter empty/stop words, and quote each term:

```typescript
private toMatchQuery(queryText: string): string | null {
  const terms = this.extractTerms(queryText);  // split on [^a-z0-9], filter stop words
  if (terms.length === 0) return null;
  return terms.map(t => `"${t.replaceAll('"', '""')}"`).join(" AND ");
}
```

### `UNINDEXED` Columns for Metadata

Columns used for filtering but not searching (like `source`, `session`) should be `UNINDEXED`. This tells FTS5 to store but not tokenize them — saves index size and prevents false matches from metadata values.

### Size Cap Check Ordering

If your store supports re-indexing the same source (delete old + insert new), check the size cap **after** deleting old data, not before. Otherwise re-indexing near capacity falsely rejects even though deletion would free enough space.

```typescript
// WRONG: rejects re-index near capacity
if (this.totalBytes + byteCount > this.maxSizeBytes) throw new Error("Cap exceeded");
this.deleteSource(source);

// RIGHT: free old space first, then check
this.deleteSource(source);
if (this.totalBytes + byteCount > this.maxSizeBytes) throw new Error("Cap exceeded");
```

## Testing Patterns

### Unique DB Paths Per Test

Prevent parallel test interference:

```typescript
function makeDbPath(): string {
  return path.join(os.tmpdir(),
    `content-store-test-${process.pid}-${Date.now()}-${Math.random().toString(16).slice(2)}.db`
  );
}
```

### Track and Cleanup All Stores

Use a module-level array + `afterEach` to prevent DB file accumulation:

```typescript
const dbPaths: string[] = [];

afterEach(() => {
  for (const dbPath of dbPaths) {
    if (fs.existsSync(dbPath)) fs.rmSync(dbPath, { force: true });
  }
  dbPaths.length = 0;
});
```

### Hook Factory Returns Rich Objects

Return `getStore()`, `getStats()`, `cleanup()` alongside the hook method. This enables testing without mocking:

```typescript
const hook = createOutputCompressionHook({ thresholdBytes: 50 });
await hook["tool.execute.after"]?.(input, output);
expect(hook.getStats().compressed).toBe(1);
hook.cleanup();  // explicit cleanup, no process.exit needed
```

### Process Exit Listener: Use `once`, Not `on`

If the hook factory can be called multiple times (tests, hot reload), `process.on("exit", cleanup)` accumulates listeners. Use `process.once("exit", cleanup)` or deregister in `cleanup()`.

# Knowledge Injection Algorithm

Canonical algorithm for injecting relevant learnings from `docs/solutions/` before phase-specific work begins. All worker workflows reference this file for the injection procedure; each workflow specifies its own keyword sources.

## Overview

Before starting main work, each phase checks the learnings index for applicable prior knowledge. This surfaces patterns, pitfalls, and institutional knowledge that previous workers documented.

**Injection must never block work.** If any step fails (missing index, invalid JSON, missing files, empty handoff data), skip silently and proceed with the phase's main work.

## Algorithm

### 1. Read the Index

```bash
cat docs/solutions/index.json
```

If the file doesn't exist or is invalid JSON, skip injection entirely — proceed to the phase's main work.

### 2. Extract Keywords

Collect keywords from the phase-specific sources (defined in each workflow file). The extraction algorithm:

1. **Collect raw text** from the specified keyword sources (see the calling workflow's keyword source table)
2. **Tokenize**: split on whitespace, `/`, `-`, `_`, and camelCase boundaries
3. **Normalize**: lowercase all tokens
4. **Filter**: remove tokens < 3 chars and common stopwords (the, and, for, with, this, that, from, into, when, will, should, would, could, also, been, have, each, etc.)
5. **Deduplicate** tokens
6. **Extract full path segments**: e.g., `packages/daemon/src/state` — keep as-is for path matching in addition to individual tokens

Also look for references to:
- Source path segments (e.g., `packages/daemon/src/state/`, `serve-manager`)
- Module names (e.g., "daemon", "controller", "worker", "state")
- Component names (e.g., "serve-manager", "decision", "fetch")
- Feature areas (e.g., "skills", "linear", "github", "review", "retro")
- Integration concerns (e.g., "PR", "labels", "MCP")
- Domain concepts and error keywords from the context

### 3. Match Keywords Against Index

Use two matching modes against the keys in `.index`:

- **Path matching**: For each key that does NOT start with `tag:`, check if any extracted keyword appears as a substring of the key (case-insensitive). Collect all matched learning file paths.
- **Tag matching**: For each key that starts with `tag:`, extract the tag name (e.g., `tag:race-condition` → `race-condition`). Check if any extracted keyword matches the tag name (case-insensitive). Collect matched learning file paths.

### 4. Deduplicate and Rank

- Remove duplicates (same file matched via multiple keys)
- **Status filter**: For each candidate, read its YAML front matter `status` field. Exclude any file with `status: superseded`. If the file doesn't exist or has no front matter, include it (graceful degradation).
- **Primary rank: tag overlap** — For each remaining candidate, read its `tags` front matter field. Count how many of its tags appear in the extracted keywords (case-insensitive). Higher overlap = higher rank.
- **Secondary rank: key specificity** — Learnings matched via longer/more-specific keys rank higher (e.g., a match on `packages/daemon/src/state` outranks a match on `packages/daemon`)
- **Tertiary rank: match count** — Number of distinct key matches (more matches = more relevant)
- **Cap at 3 learnings maximum**

### 5. Read Matched Learnings

For each matched learning file (from `docs/solutions/<path>`):

1. Read YAML front matter: extract `title` and `tags` fields
2. Skip past front matter (`---` blocks) and headings, take the first paragraph of prose (typically the Problem or Overview section)
3. Prepend structured header: `[{title} | tags: {comma-separated tags}]`
4. Truncate entire output (header + prose) to **350 characters**

**If a matched file doesn't exist on disk:** Skip that entry silently (stale index entry from a file rename). Do not error.

### 6. Output Injected Learnings

Output the injected learnings visibly in the session before proceeding with the phase's main work:

```
## Relevant Learnings (from docs/solutions/)

1. [docs/solutions/<path>]: [{title} | tags: {tag1}, {tag2}] <prose excerpt> (350 chars max total)
2. [docs/solutions/<path>]: [{title} | tags: {tag1}, {tag2}] <prose excerpt>
3. [docs/solutions/<path>]: [{title} | tags: {tag1}, {tag2}] <prose excerpt>

(Review these for patterns and pitfalls relevant to this phase's work.)
```

**If no matches found:** Output "No relevant learnings found." and proceed. Do NOT add an empty section.

**Canonical identifiers:** All references to learnings use their `docs/solutions/` relative file path (e.g., `daemon/controller-lifecycle-separation.md`). These paths are the stable IDs used for injection, handoff tracking, and future aggregation. Never use titles or truncated text as identifiers.

## Fallback Behavior

When a keyword source is unavailable (missing handoff data, empty fields, missing phase data), silently fall back to the next available source as defined in the calling workflow's fallback rules. Never error on missing data.

## Integration with Handoffs

If the phase writes handoff data, include a `learningsUsed` field listing the `docs/solutions/` relative paths of all injected learnings. This enables downstream phases to see what knowledge was available and supports future aggregation.

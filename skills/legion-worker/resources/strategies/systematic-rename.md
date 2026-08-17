# Strategy: Systematic Rename

When a repo, package, or URL is renamed across a codebase.

## Checklist

1. **Scope by file type** — grep all text-bearing extensions, not just the obvious ones:
   - Source code (`.ts`, `.js`, `.py`, `.sh`) — functional, must update
   - CI/CD configs (`.yml`, `.yaml`) — functional, must update
   - Documentation (`.md`) — correctness, should update
   - Config files (`.json`, `.toml`) — check but may be immutable

2. **Classify matches as mutable vs immutable** — historical records (transcripts, test snapshots, progress logs) must NOT be modified. Changing them falsifies history.

3. **Check comments for semantic context** — a comment mentioning the old name may still be correct in intent. Update the name but preserve the reasoning.

4. **Verify with grep before AND after** — capture pre-edit state as a baseline for comparison.

5. **Use `jj diff --git`** for verification — plain `jj diff` without color concatenates old/new text confusingly (e.g., `old-nameNEW-name` without color codes).

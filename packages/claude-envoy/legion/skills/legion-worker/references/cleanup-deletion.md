# Strategy: Cleanup & Deletion PRs

When deleting deprecated code, stale docs, or consolidating references.

## When deleting a CLI command, check all four:

1. Implementation file(s)
2. `package.json` / `pyproject.toml` script entry
3. All project documentation references (AGENTS.md or equivalent — check command tables AND section headings)
4. Wrapper scripts or CI jobs that invoke it

## project doc headings are documentation too

When updating a command reference, grep for the section heading and update it in the same commit. Headings that reference specific paths (`## Foo (meta/bar/)`) go stale when paths change.

## Deletion PRs should be almost entirely deletions

Resist opportunistic refactors. If the diff has significant additions, the scope has crept. The value of a cleanup PR is its tight, reviewable scope.

## Complete the deletion chain

If a feature has implementation + CLI wrapper + package.json entry + docs, remove all of them together. Partial deletion leaves broken references.

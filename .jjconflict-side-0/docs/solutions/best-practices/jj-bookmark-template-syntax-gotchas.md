---
title: "jj bookmark template syntax gotchas"
category: best-practices
tags:
  - jj
  - version-control
  - jj-workspaces
  - bookmark-parsing
  - template-syntax
date: 2026-04-13
status: active
related_issues:
  - "489"
symptoms:
  - "jj bookmark list template returns unexpected output"
  - "tracking_ahead_count comparison not working in jj template"
  - "jj bookmark shows remote:git and remote:origin entries"
---

# jj bookmark template syntax gotchas

## `tracking_ahead_count` is a SizeHint, not an integer

In jj template language, `tracking_ahead_count` returns a `SizeHint` type, not a plain integer or
boolean. Using it directly in an `if()` guard doesn't work as expected.

**Wrong:**
```
if(tracking_ahead_count, ...)
```

**Correct — use `.lower()` to convert:**
```
"ahead:" ++ tracking_ahead_count.lower()
```

This produces parseable output like `ahead:3` or `ahead:0`. The `.lower()` method extracts the
numeric lower bound from the SizeHint.

## `remote:git` vs `remote:origin`

When using `jj bookmark list --all`, the output includes entries for both remotes:
- `remote:git` — the colocated git backend (local)
- `remote:origin` — the actual remote

**Always filter for `remote:origin`** when checking push status. Without this distinction, you'd
falsely conclude a bookmark is pushed just because it's tracked by the local git backend.

Example template that outputs parseable lines:
```
name ++ if(remote, " remote:" ++ remote ++ " ahead:" ++ tracking_ahead_count.lower(), " local") ++ "\n"
```

Output:
```
my-branch local
my-branch remote:git ahead:0
my-branch remote:origin ahead:3
```

## Case normalization

jj bookmarks are stored lowercase. When looking up a bookmark by issue ID (which may contain
uppercase characters), always `.toLowerCase()` the input before passing it to
`jj bookmark list`. Missing this causes false "no bookmark found" results.

## Takeaway

When writing jj bookmark templates for programmatic parsing:
1. Use `.lower()` on `SizeHint` types (`tracking_ahead_count`, `tracking_behind_count`)
2. Filter for `remote:origin` specifically — ignore `remote:git`
3. Normalize case before bookmark lookup
4. Design templates for machine parsing (structured output with delimiters, not human-readable)

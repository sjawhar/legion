---
title: ".legion/ Handoff File Conflicts During Concurrent Rebases"
category: legion
tags:
  - legion
  - jj
  - conflict-resolution
  - handoff
date: 2026-04-12
status: active
module: legion
related_issues:
  - "sjawhar-legion-423"
  - "273"
symptoms:
  - "Rebase onto main causes .legion/plan.json conflicts"
  - "Resolving conflict in ancestor commit re-triggers conflict in descendant"
  - "Multiple rounds of conflict resolution needed for same .legion/ file"
---

# .legion/ Handoff File Conflicts During Concurrent Rebases

## The Pattern

When rebasing a feature branch onto main after a concurrent issue merges, `.legion/plan.json` conflicts across **multiple ancestor commits**. Resolving the deepest ancestor causes descendants to re-conflict due to jj's automatic descendant rebasing.

This happens because `.legion/plan.json` lives at a fixed path but contains per-issue data. Every issue branch writes different content to the same file, so any rebase after a concurrent merge creates a conflict.

## Example (Issue #423)

Issue #240 merges to main. Issue #423 rebases onto main. Two ancestor commits (plan phase and implement phase) both have `.legion/plan.json` conflicts:

```
@  (empty, working copy)
×  wytu — conflict in .legion/plan.json
×  vuvz — conflict in .legion/plan.json
◆  main — includes #240's plan.json
```

Resolving `vuvz` (deepest) causes jj to auto-rebase `wytu`, which re-conflicts because `wytu` was based on the pre-resolution `vuvz`.

## Resolution: Edit-and-Squash, Bottom-Up

1. **Start at the deepest conflicted commit**: `jj new <deepest-conflict>`
2. **Edit the conflicted file** to keep the current branch's version (overwrite conflict markers)
3. **Squash into the conflicted commit**: `jj squash -u`
4. **Move to the next conflict** (which re-appeared from the auto-rebase): `jj new <next-conflict>`
5. **Repeat** edit → squash for each descendant
6. **Verify**: `jj agent-log` — all commits should show `"conflict":false`

The key insight: use `jj new <commit>` + edit file + `jj squash -u`, NOT `jj resolve`. The file contains per-issue data where one side is always correct — just overwrite with the right version.

## Escape Hatch: Fresh Branch from Main

When `.legion/` conflicts are too deep to resolve — especially after multiple worker re-attempts
that each wrote different handoff state — it may be faster to abandon the conflicted branch and
re-create from a clean main:

1. Save the implementation code (copy files or note the commit)
2. Create a new change on main: `jj new main`
3. Apply the same code changes
4. Create a new bookmark and push: `jj bookmark set <new-name> && jj git push --named <new-name>=@`
5. Open a new PR (close the old one)

This was validated in #273 (PR #511 → #525): identical code, clean branch, same-day turnaround.
The only cost is a new PR number. Use this when:
- Multiple ancestor commits have `.legion/` conflicts from prior failed attempts
- The resolve-in-place approach (bottom-up edit-and-squash) would take longer than re-applying the code
- The code diff is small relative to the conflict surface

## Prevention

- **Squash before rebasing**: If the branch has multiple commits touching `.legion/`, squash them into one first. One conflict to resolve instead of N.
- **Rebase frequently**: Don't let branches diverge from main for long.
- **Merge shorter-lived branches first**: Reduces the conflict window.

## When This Isn't a Problem

- Single-implementer workflows (only one branch at a time)
- Sequential merges (no concurrent work)
- Issue only has one commit touching `.legion/plan.json`

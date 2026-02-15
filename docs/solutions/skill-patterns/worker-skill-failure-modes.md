---
title: "Worker Skill Failure Modes and Fixes"
date: 2026-02-15
category: skill-patterns
tags: [workers, skills, implement, review, merge, retro, failure-modes]
related-issues: [LEG-122, LEG-125, LEG-128, LEG-129, LEG-130]
---

# Worker Skill Failure Modes and Fixes

Observed during the first full controller integration test. Each failure mode led to a skill update.

## Implement Workers

### jj abandon destroys work

**Symptom:** Worker used `jj edit @-` to go back to parent, then `jj abandon @` which destroyed all changes.

**Root cause:** After `jj edit @-`, `@` points to the parent commit, not the work commit. `jj abandon @` kills the parent.

**Fix:** Added jj safety rules to legion-worker SKILL.md:
- Always `jj new` for isolated commits
- Never `jj abandon` without `jj log` first
- Use `jj op restore` to recover

### Branch ancestry pollution

**Symptom:** PR included 21 files instead of 8. Unrelated parent commits were in the ancestry.

**Root cause:** Worker was created on a workspace that had accumulated changes from multiple issues.

**Fix:** Added ancestry verification to implement.md step 6 (Ship):
- `jj log -r 'ancestors(@, 5)'` before PR creation
- `jj diff --stat --from main` to check file count

### Address-comments with no comments

**Symptom:** Worker dispatched for "address comments" but reviewer had converted PR to draft without posting specific feedback. Worker had nothing to address.

**Fix:** Added check to implement.md Mode 2 step 1: if zero review comments exist, rebase and exit.

## Review Workers

### Draft status not set after finding critical issues

**Symptom:** Reviewer found P1 CRITICAL issue but did not convert PR to draft. PR was merged with the blocker.

**Root cause:** The draft conversion step wasn't emphasized as mandatory.

**Fix:** Strengthened step 5 in review.md: every review MUST set draft status.

### No review comments posted when requesting changes

**Symptom:** Reviewer converted PR to draft (requesting changes) but posted no specific comments. Implementer had nothing to address.

**Root cause:** No explicit requirement that changes-requested must include specific feedback.

**Fix:** This is implicit in the workflow (steps 3 and 4 post comments before step 5 sets status), but could be strengthened with an explicit check.

## Merge Workers

### PR already merged

**Symptom:** Merger spent 11 messages investigating why it couldn't merge a PR that was already on main (merged via a different PR).

**Root cause:** No pre-merge check for PR state.

**Fix:** Added step 1 to merge.md: check PR state. If already merged, exit cleanly.

### External repo permission error

**Symptom:** Merger couldn't merge PR on upstream repo (obra/streamlinear), kept retrying.

**Root cause:** No permission to merge on external repos.

**Fix:** Added permission error handling to merge.md step 6: escalate with `user-input-needed` instead of retrying.

## Retro Workers

### Wrong session (fresh instead of resumed)

**Symptom:** Retro worker got confused by `LINEAR_ISSUE_ID=test-1` environment variable and couldn't find the PR.

**Root cause:** Controller dispatched a fresh implement worker instead of resuming the original implement session. The fresh session had no context.

**Fix:** This is now the standard retro workflow — see `legion-retro/SKILL.md` and the controller skill's dispatch table. Retro MUST resume the original implement session. If the worker died, a fresh dispatch loses the implementer's perspective.

### Learnings not committed

**Symptom:** Retro wrote docs/solutions/ files but didn't commit or push them.

**Fix:** Added explicit commit+push step to legion-retro SKILL.md before posting summary to Linear.

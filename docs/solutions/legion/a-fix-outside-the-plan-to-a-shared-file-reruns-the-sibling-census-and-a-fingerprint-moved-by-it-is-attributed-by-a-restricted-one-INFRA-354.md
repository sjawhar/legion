---
title: "A fix outside the plan to a shared file runs the sibling census again, and a fingerprint it moves is attributed by a restricted one"
category: legion
tags:
  - forward-merge
  - unchanged-diff-fingerprint
  - sibling-pull-request
  - jj
  - conflict-resolution
  - documentation
date: 2026-10-25
status: active
module: docs
related_issues:
  - "INFRA-354"
  - "sjawhar/legion#2185"
  - "INFRA-234"
  - "sjawhar/legion#2182"
---

# A fix outside the plan to a shared file runs the sibling census again, and a fingerprint it moves is attributed by a restricted one

Extends docs/solutions/legion/sibling-pr-rewrites-your-function-spell-both-shapes-in-the-plan-and-expect-the-fingerprint-to-change.md.

That document covers sibling pull requests known when the plan is written, for files the plan
names. This one covers what changes when the scope grows during implementation.

- When a run forces you to fix a file that is not in the plan's task list (for example, a
  documented recipe your proof depends on, which turns out to be wrong), list the open pull
  requests that touch that file **before you push the fix**, then read the ones that do:
  `gh pr list -R <owner>/<repo> --state open --limit 200 --json number,files --jq '[.[] | select(any(.files[]; .path == "<path>")) | .number]'`.
  A busy file has many hits (`docs/kubernetes.md` had 13 open, #2185 included, on 2026-10-25), so read each one's
  diff for the same hunk. When a sibling already carries the fix, keep it out of your diff and rely
  on theirs. Otherwise, record the overlap the way the plan records known siblings.
- If you miss it and the sibling merges first, the forward merge takes the sibling's text and your
  whole-diff fingerprint moves. Attribute the move with **a restricted fingerprint**: the same
  command with that file added to the fileset's exclusion. Then show that the file's own delta only
  **removes** lines: no line at the merged head is missing from your earlier delta. With both
  measurements, the move is attributed by a calculation rather than by an explanation
  (`docs/solutions/legion/unchanged-diff-fingerprint-one-fileset-verified-by-a-pair.md` has the
  restricted form). Say in the pull request body that the removal is not a retraction: another
  issue found the same defect and its pull request landed first.
- Resolve such a file by hand, or by taking only the conflicting hunks. In jj 0.45,
  `jj resolve --tool :theirs <file>` replaced the **whole file** with main's side. Your
  non-conflicting hunks in the same file are gone, and `jj diff --from main@origin -- <file>` shows
  nothing. After any tool-side resolution, check for each of your own hunks in that file.

## Evidence

- INFRA-354's proof needed a Postgres, started as a cluster Job by following
  `docs/kubernetes.md` "Running a workload in the cluster from a role". The namespace's admission
  refused the documented Service, which selected by `job-name`; it requires
  `batch.kubernetes.io/controller-uid`. The implementer fixed the page in sjawhar/legion#2185, a
  fix outside the plan's task list. The handoff's `discoveredComplexity` named only the sibling
  known at plan time (#2162).
- sjawhar/legion#2182 (INFRA-234), which changes `docs/kubernetes.md`, was opened at 06:51Z. The
  implementer met the refusal at about 07:08Z, so a census would have found it. #2182 carried the
  same selector fix and a dated run record, and it merged at 08:54Z, after #2185's approval at
  3c5b8aaef2e4. GitHub then reported #2185 `CONFLICTING`.
- In the forward merge (9cdb4130c10a), the whole-diff fingerprint moved from `2f2e9983…` to
  `b874ad58…`. The fingerprint over `~(.legion | docs/solutions | docs/kubernetes.md)` was
  `c74431614bd5…` both before and after. The `docs/kubernetes.md` delta dropped 59 lines and added
  none. The tester reproduced both fingerprints. The reviewer found the diff outside that file
  byte-identical to the head it had approved, and approved the merged head.
- During that merge, `jj resolve --tool :theirs docs/kubernetes.md` dropped the branch's two
  `BROKER_SERVICES` passages, which were outside the conflicted section. A grep for one of them
  found the loss, and the two hunks were applied again from the branch's own diff before the push.

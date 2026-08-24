---
name: legion-retro
description: Use when an issue has passed review and its parked implementer is revived for the mandatory pre-merge Legion retrospective.
---

# Legion Retro

Retro is mandatory for every issue that passed review. The architect revives the parked
implementer so the person with implementation context performs the retrospective, and the
skill obtains a separate fresh-eyes perspective. Retro runs before merge.

## Merge-gate ordering

Follow this ordering exactly. It keeps the reviewed branch clean while preserving the
retrospective's durable output.

1. Tester green and all code-review cycles finish.
2. The reviewer removes `.legion/`, pushes that deletion as its final commit, then approves.
3. Run this retro: commit durable learnings to `docs/solutions/` and post the issue comment.
   Retro writes **no `.legion` file**, so it never re-dirties the cleaned handoff tree.
4. Sami approves the final reviewed head.
5. The merger squash-merges and pushes nothing.

Do not start retro before step 2, skip it because the change seems mechanical, or merge before
steps 3 and 4. The design gate is not a substitute for this final merge gate.

## Two perspectives

1. Re-read the issue, its acceptance criteria, the PR, test evidence, and review evidence.
   Do not rebase or create a new branch; work on the existing issue branch.
2. Spawn one fresh-eyes subagent. Give it the issue and PR, ask it to inspect the diff and
   return concrete reusable learnings, and require it to return analysis rather than edit files.
3. Independently record the implementer's perspective: surprising constraints, difficult
   decisions, failed approaches, and reusable patterns.
4. Integrate the two perspectives. The implementer owns the final judgment: reject generic or
   context-free suggestions and preserve only learning that will help a future worker.

## Durable outputs

Write the integrated learning as one or more discoverable documents under `docs/solutions/`.
Organize by reusable topic rather than by pull request. Each document uses this front matter:

```yaml
---
title: "Descriptive title matching the H1"
category: subdirectory-name
tags:
  - searchable-topic
date: YYYY-MM-DD
status: active
module: affected-module
related_issues:
  - "owner/repo#123"
---
```

Commit the documentation on the existing issue branch, advance its existing bookmark, and push
that branch. Do not create a replacement branch or bookmark. Then post an issue comment naming
the documents and the one-to-three most useful takeaways. The comment must carry this revived
implementer's structured attribution footer with `phase` set to `retro`:

```bash
legion gh -- issue comment <issue-number> \
  --body $'## Retro Complete

**Learnings documented in:**
- docs/solutions/<path>.md

**Key takeaways:**
- <reusable lesson>

<!-- legion: {"session":"<session-id>","phase":"retro"} -->' \
  --repo <owner>/<repo>
```

The issue comment and `docs/solutions/` commit are the only retro outputs. Never write a
handoff, phase artifact, local feedback log, or completion label.

## Completion check

Before returning, verify all of the following:

- The reviewer cleanup commit remains below the retro documentation commit.
- The learning documents and issue comment both exist.
- No `.legion` file was created or modified by retro.
- The fresh-eyes analysis was considered alongside the implementer's context.
- Sami's approval and the merger remain subsequent steps, not work performed by retro.

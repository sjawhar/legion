---
title: "A count in a workflow comment is recomputed from the file, never supplied by eye"
category: github
tags:
  - workflows
  - path-filters
  - review
  - comments
date: 2026-11-03
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2641"
---

# A count in a workflow comment is recomputed from the file, never supplied by eye

Extends docs/solutions/github/a-workflows-own-trigger-list-serves-every-job-under-it-and-usually-starts-no-run-for-its-own-change-pr2607.md,
which is about the trigger list a workflow's own change does not run. This is about the labels a
reader uses to check such a list.

## The rule

- **A comment that states a count is a claim about the file, and no program checks it.**
  `.github/scripts/check-ci-filters.sh` compares the pattern *lists* and exits 0 whatever a label
  says, so a wrong number survives every gate and misleads exactly the reader who trusts it.
- **Recompute it with a command, quote the command, and quote the range it counted.** A count
  supplied by eye in a review is as likely to be wrong as the one it corrects: in this issue a
  reviewer's round-1 correction replaced 57 with 59 and 42 with 50, and nine rounds later the
  file's own lines were 60 and 53, so the corrections had been miscounts too.

  ```bash
  sed -n '<first>,<last>p' .github/workflows/<file>.yaml | grep -c "^      - '"
  ```

- **A label corrected without counting is corrected again.** All three labels of this file were
  wrong at once, and one pull request fixed each in a round of its own: `*bun_install` to 15
  (`29b084da`), then `*legion_command` to 60 and `*dispatch_server` to 53, eight rounds later.
  The first two of those three corrections were themselves supplied by eye, as 59 and 50, and
  both were wrong; the one that was counted, 15, was right.
- **Prefer a label that names the anchor it flattens over one that counts it.** The count is a
  maintenance cost that main's next addition pays: `*legion_command` went from 59 to 60 because
  main added `packages/daemon/internal/handoffimport/*`, in a pull request that had no reason to
  look at this comment.

## Evidence

- `.github/workflows/docs-pages.yaml:61` and `:122` in sjawhar/legion#2641: labels corrected to
  `*legion_command, 60 patterns` (lines 62-121, counted) and `*dispatch_server, 53 patterns`
  (lines 123-175, counted), the three remaining `*dispatch_server` entries of ci.yaml's 56 sitting
  later in the file under the docs filter's own section.
- `.github/scripts/check-ci-filters.sh` exited 0 both before and after that change, reporting
  `every ci.yaml filter matches every file its job reads (28 gates)` and
  `docs-pages.yaml: on.push.paths equals filter docs (174 patterns)` — evidence that only the
  labels moved, and no evidence at all that the labels are right.
- The history: #2641's review round 1 asked for 59 and 50; its round 10 tester counted the file
  and got 60 and 53.

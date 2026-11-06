---
title: "After a forward merge, rerun the suites no required job runs, and date every proof to the merged head"
category: github
tags:
  - forward-merge
  - required-checks
  - proof
  - merged-tree
date: 2026-10-31
status: active
module: .github
related_issues:
  - "sjawhar/legion#2354"
---

# After a forward merge, rerun the suites no required job runs, and date every proof to the merged head

Extends docs/solutions/github/a-base-gate-arrives-in-the-merge-and-the-job-your-branch-alone-adds-is-the-line-it-refuses-pr2517.md.

That note says the base's own commit usually carries a moved pin or count with the change that
needed it, so the merge closes it. It does not when the base changes the pinned thing in one
pull request and the pin in another, and your merge lands between them.

- **Rerun first the suites of every package the merge touched that no required job runs.** A pin
  or count the merge broke goes red only there, and the required checks stay green. Read which
  jobs are required from the base's rules, as
  `docs/solutions/testing/a-regression-lock-binds-only-as-far-as-its-lane-is-required-INFRA-149.md`
  says.
- **Date each proof claim to the head it ran at.** A suite result from before the merge says
  nothing about the merged head. Run it again there, or name the head it ran at.
- **Read your stand-ins against the merged code.** When the merge adds a read to code your test
  drives, a test can keep passing only because its stand-in answers the new read with a 404.
  Serve the read and assert what the code does with it.

## Evidence

On sjawhar/legion#2354, merge a5f36189 brought main's #2478, which added
`GET /api/v1/github/app-installation/{owner}/{repo}` to Dispatch while main's route-count pin
stayed at its old value. The merged `routes_table_test.go` pinned 150 routes against 151 rows,
so `go test ./internal/dispatch/api` failed. The required `test` job does not run that package,
and all required checks passed. The PR body said "the full Dispatch API package passes", from a
run at 4d3b8346, before that merge. Review round 12 found both. Commit cae0d441 set the pin to
151 (main's 147 plus this change's 4), and the PR body was corrected.

The same merge made `TestThePassFollowsAPushAboveAnApprovalAsANewHead` pass only because its
GitHub stand-in answered #2478's new branch-activity read with a 404. Commit b036d227 serves an
activity entry and asserts the push the pass rebuilds; with the read answering 404, the test
fails.

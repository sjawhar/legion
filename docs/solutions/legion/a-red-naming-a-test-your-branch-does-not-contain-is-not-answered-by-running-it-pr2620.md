---
title: "A red naming a test your branch does not contain is not answered by running it"
category: legion
tags:
  - ci
  - forward-merge
  - merge-ref
  - go-test
date: 2026-11-01
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2620"
---

# A red naming a test your branch does not contain is not answered by running it

Extends docs/solutions/legion/a-red-check-in-a-package-the-branch-does-not-touch-is-mains-until-mains-own-run-says-otherwise-pr2580.md.

- **Step 2 of that page assumes the test is in your tree.** When it is not,
  `go test <pkg> -run '<Name>'` at your head prints `ok … [no tests to run]` and exits 0. That is
  not the pass step 2 reads as "the two trees differ"; it discriminates nothing, and a worker who
  takes it as a pass has the right conclusion from the wrong evidence.
- **Ask whether the file exists instead.** Absent at your head and present at the base's tip is
  the answer step 2 wants, and it is unambiguous:
  `jj file list -r <head> 'root:<package>'` against
  `jj file list -r <base>@origin 'root:<package>'`, or a `grep` for the test's name in each. Then
  go on to step 3 and take the verdict from the base's own push run.
- **Run the test for real only after the forward merge brings it in**, and record that run. Before
  the merge there is nothing at your head to run.

## Evidence

sjawhar/legion#2620. `ci-ok` was red at head `e9e218e8312c` through `envoy-go` alone (run
41331799009, job 124923436999), on
`--- FAIL: TestMigrationSetTakesNumbersThenTimestamps` with
`FAIL github.com/sjawhar/envoy/internal/dispatch/store 191.875s`.

`go test ./internal/dispatch/store/ -run TestMigrationSetTakesNumbersThenTimestamps` in the
branch's own workspace answered `ok … [no tests to run]`, exit 0. The test was not in the branch
at all: `main` had added it while the branch's base was older, and CI tests the merge of the
branch with the base, which is where it existed. The file question settled it — the package's
test files at `main@origin` carried it and the branch's did not.

The rest followed pr2580 unchanged: the fix landed on `main` in its own pull request (#2633,
merged as `0642a37a`), one forward merge cleared the red (`64ecd4d515b0`, no conflict,
fingerprint `eb443e84…f96c` equal at both heads), the change's own proof was re-run at the merge
head, and the test ran for real there for the first time.

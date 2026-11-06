---
title: "A test main breaks, which another open pull request fixes, is copied byte for byte after two checks"
category: testing
tags:
  - flaky-test
  - required-check
  - red-on-main
  - race-detector
date: 2026-10-31
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2387"
---

# A test main breaks, which another open pull request fixes, is copied byte for byte after two checks

Extends docs/solutions/dispatch-web/a-merge-of-main-can-add-a-writer-of-an-event-the-narrowed-rules-know-and-a-test-red-on-main-pr2324.md.

That note covers a test main added that fails on your merged head, in code your pull request owns.
This one covers a test your pull request does not touch that fails on main too and blocks your
required check, while another open pull request already fixes it.

- **Show that the failure is main's.** Run the test on an export of main and at your head the same
  way, many times for a flaky one: `go test -race -count=50 -run '<Test>$' <pkg>`. Show that your
  diff does not touch the test or the code it drives. Record both counts.
- **Look for an open fix before you write one** (`gh pr list --search '<test name>'`, or the
  controller). Copy its changed file whole, so that pull request still merges cleanly over yours
  and the two never carry different fixes of one test.
- **Check two things first.** Your branch has no edits of its own in that file, or the copy drops
  them silently. The files the test reads can differ on your branch, so run the copied test against
  your branch's copy of them and against main's: CI runs the merge of your head with main.
- **Prove it.** The copied file's hash equals the source pull request's head. The test passes after
  the copy, and the old file fails the same way CI failed. Tell your architect that main has the
  broken test, and name the fixing pull request, so the controller hears it.

## Evidence

In sjawhar/legion#2387, `TestAReconnectCatchesUpWhatTheListenerMissed` turned `envoy-go-race-api`
and `ci-ok` red. Under `-race -count=50` it failed 1 of 50 on main `f4ec92ee` and 2 of 50 at the head,
in a file the pull request did not change. Commit `f9f6188d` took #2594's
`delivery_live_hub_test.go` (blob `f5d97ae2`), and 50 of 50 passed. Later `envoy-go` failed
`TestTheDocumentedClaimsAreTheClaimsAnExchangedTokenCarries`, which main had broken when #2474
relaid `docs/kubernetes.md`. Commit `fd7b9b1b` took #2598's `claims_doc_test.go`. That pull request's
head held a different `docs/kubernetes.md` from this branch, so the test ran against main's guide
and the branch's. It passed against both, and the old file failed against main's guide with CI's
message.

---
title: "A forward merge owes every gate your branch's own lines feed, and the tests main just added"
category: legion
tags:
  - forward-merge
  - jj
  - ci-filters
  - migrations
  - long-lived-pr
date: 2026-11-01
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2198"
---

# A forward merge owes every gate your branch's own lines feed, and the tests main just added

Resolving the conflicted files is the smaller half of a forward merge. The rest of the merge is
silent: `main` renames a package your branch's own CI line names, or lands a test that your
branch's behaviour refuses, and nothing in the merge output says so.

- **Re-run every hand-kept gate your branch feeds, not only the migration-numbering guard.** A
  filter line, a read model, a census table: each is a list a person maintains, and a rename on
  `main` turns your entry into a path that matches nothing.
- **Run the test files `main` added or changed over your own surface, whatever conflicted.** A
  merge with no conflicting hunk in Go code can still land a test that drives the route your
  change closed, or builds a configuration your change makes incomplete.
- **Adapt a foreign test through the helper your own tests use**, and say which assertions you
  kept and which the new route cannot make. A claim that "the assertions are untouched" is part
  of the diff and is read as one.
- **Measure a pod-only failure set at the head you name, by test name, against the same packages
  on `main`.** The count carried from an earlier round is a number nobody measured.

## Evidence

In sjawhar/legion#2198, two forward merges three days apart:

- `main` moved `internal/broker/ratelimit` to `internal/ratelimit`. The branch's own `dispatch`
  filter line in `.github/workflows/ci.yaml` named the old path, and
  `.github/scripts/check-ci-filters.sh` refused it: "matches no file in the repository, so no
  change ever selects on it". Nothing in the merge conflicted there.
- The second merge had three conflicts, none in Go, and two of `main`'s new tests were red at the
  merge head: `TestANarrowedVMsPendingRequestGrantsNothingWhenApproved` approved a secret request
  through the account route this branch refuses for every caller with `403
  DECIDE_ON_BROKER_PAGE`, so the request stayed pending; and `cmd/broker/vm_identity_test.go`'s
  `vmConfig` built a broker configuration without the four `BROKER_SIGNIN_*` settings this branch
  makes required, so all four of its tests died at `config.Load`.
- Adapting the first was one line, `ts.decide`, because the branch had already given the shared
  test package a helper that decides a secret request on the broker's page; the merge comment said
  the assertions were untouched, and the tester found the adaptation had also dropped a check on
  the old route's own answer.
- The same comment carried "18 pod failures" for `cmd/agent-secrets` from an earlier round's
  measurement; at that head the number was 13, the same 13 as on `main`.

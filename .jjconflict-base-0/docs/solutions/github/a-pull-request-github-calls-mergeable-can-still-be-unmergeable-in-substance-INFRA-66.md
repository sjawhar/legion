---
title: "A pull request GitHub calls mergeable can still be unmergeable in substance"
category: github
tags:
  - merge
  - long-lived-branch
  - ci
  - semantic-conflict
date: 2026-10-23
status: active
module: packages/envoy
related_issues:
  - "INFRA-66"
  - "sjawhar/legion#2087"
---

# A pull request GitHub calls mergeable can still be unmergeable in substance

- `mergeable` means no two lines collide. It says nothing about whether the merged tree compiles,
  or whether the base's new tests still pass against your change.
- The head CI judges is the **merge**, not your branch's own head. A branch whose base moved
  under it is merged and re-driven, not trusted because no line collided.
- Expect two shapes, and check for both: a deletion only the merged tree's **compiler** sees, and
  a new test on the base that **not even the compiler** sees.
- A behaviour change made inside a merge's conflict resolution is invisible to anyone reading the
  commit list. Describe it in the merge commit's message and in the pull request body, where a
  commit list is not the only index.
- A forward merge keeps the fingerprint discipline: record the unchanged-diff fingerprint before
  and after, post both, and say plainly when the diff legitimately **changed** — a merge that had
  to fix the base's new breakage is the one case where "unchanged" would be the lie.

## Evidence

A branch open for several days was reported mergeable throughout and was green on its own head
every round. Merging the base revealed both shapes at once:

- The base had moved two constants into another package and deleted them from theirs. The branch
  named the deleted constants in code it had added, so the **merged** tree did not compile; two
  CI jobs failed on it.
- The base had also added a live test that the branch's own change broke: a single lease covering
  two unrelated NATS durables, which made one durable's denied permission the other durable's
  outage. No compiler sees that one, and the branch's suite did not contain the test.

Both were fixed inside the merge commit, whose message says so, because the second was a real
behaviour change — a per-durable lease name — that a reader of the commit list would otherwise
meet only as "merge".

The fingerprint comment for that merge reads `changed`, with the two reasons under it, and a note
that the template word "rebase" is not what was done: it was a forward merge with both parents
named, no rebase, no rewrite, an ordinary fast-forward push.

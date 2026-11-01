---
title: "A later round that changes no runtime code carries the runtime proofs by a reproducible binary hash"
category: best-practices
tags:
  - reproducible-build
  - proof
  - review-round
  - go
date: 2026-10-28
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2467"
---

# A later round that changes no runtime code carries the runtime proofs by a reproducible binary hash

- When a review round changes only tests, comments or docs, do not re-run a long runtime proof (a
  replay, a load test) to show nothing changed. Build the shipped binary reproducibly at the commit
  the proof ran on and at the new head, and compare the hashes: equal hashes carry the proof over.
- Build each commit from an export of that commit alone (`git archive <commit> go.work go.work.sum
  <module dir> | tar -x`), with `GOWORK=off go build -trimpath -buildvcs=false -ldflags=-buildid=`.
  Without those flags the binary embeds paths, the VCS state and a build id, and two builds of the
  same source differ.
- Check that each build succeeded before you hash it, and use the base commit as the negative
  control: its hash must differ, or the comparison cannot tell anything apart.
- A source diff that touches only `_test.go` files and comments says the same thing more weakly; the
  hash also covers generated code, build tags and embedded files.

## Evidence

- In sjawhar/legion#2467 the replay, EXPLAIN and equivalence proofs ran on runtime code `b0d5039f`.
  Review round 2 changed only test helpers and comments (`63ebec0987`, `a0a78bdcd3`). The tester
  built `cmd/dispatch` from archives of `b0d5039f` and of `30b75cfd`: both hashed to `a1102d47…`,
  and base `3168cd8c` hashed differently. The reviewer accepted the earlier runtime proofs at the
  new head on that basis, with no new replay run.

---
title: "A generator that parses source loses an answer you move, and the CI read model loses the directory you add"
category: github
tags:
  - broker-refgen
  - docs-generators
  - ci-filters
  - ci_job_reads
  - go-ast
date: 2026-11-01
status: active
module: packages/envoy/cmd/broker-refgen, .github/scripts
related_issues:
  - "sjawhar/legion#2198"
---

# A generator that parses source loses an answer you move, and the CI read model loses the directory you add

A generator that reads the source with `go/ast` and runs no type checker sees calls it can
resolve by name in the package it is parsing, and nothing else. Moving an answer out of that
package deletes it from the published reference with no error anywhere.

- **Re-run the generator and `diff -r` its output whenever you move or re-shape an answer**, not
  only when you add a route. The page is generated at site build, so a wrong page costs nothing
  until a reader believes it.
- **Where the generator cannot resolve the call, name the method and its package in a table**,
  say in that table's own comment why a table is needed, and resolve the callee in that package's
  own graph so the answer still comes from the real code.
- **Add the assertion that fails when the answer goes missing again**, at the level the reference
  prints it.
- **A new parsed directory, a new file read through `..`, a new `go` command: each owes
  `.github/scripts/ci_job_reads.py` its entry**, whatever the filters already cover. The gate
  proves the filters cover the model, not that the model is complete, so an unmodelled read
  leaves a glob nothing requires and a later narrowing drops it with the gate still green.

## Evidence

In sjawhar/legion#2198 the broker's rate-limit refusal for the approval page's routes moved from
`writeError` in the API package to `approval.Page.WriteRateLimited`, reached through the field
expression `s.deps.Approval`. `cmd/broker-refgen` reads a credential class's refusals from
`authenticate`'s case for that class and resolves calls inside the API package alone, so the
generated `api.md` listed the "Broker sign-in session" class's refusals as `none` while that
class's own documented text said every route of it spends the bucket first. Nothing in refgen's
tests caught it.

The fix is `pageAnswerers` in `cmd/broker-refgen/routes.go`, one entry naming `WriteRateLimited`
and `internal/broker/approval`, with `resolve` reading the callee in that package's graph, so the
429 comes out of the method's own `WriteHeader`; `TestTheSignInClassRefusesPastTheRateLimit` fails
with "the Broker sign-in session class refuses \[\], want the approval page's 429" when the two
files are reverted.

That fix then made refgen parse a sixth directory while `ci_job_reads.py`'s `BROKER_REFGEN_DIRS`
and the docs job's static reads still named five. `check-ci-filters.sh` stayed green, because the
docs filter carries that directory for the broker binary's own build closure, so the gap was
invisible: what was lost is the proof, which is what the check exists for. Its own header says a
change that starts a new read adds it to that model.

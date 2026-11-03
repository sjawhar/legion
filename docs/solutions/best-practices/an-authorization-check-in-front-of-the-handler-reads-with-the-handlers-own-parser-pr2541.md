---
title: "An authorization check in front of the handler reads with the handler's own parser, and says what licenses it to read early"
category: best-practices
tags:
  - authorization
  - postgres
  - uuid
  - fail-closed
  - deploy-order
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch/api
related_issues:
  - "sjawhar/legion#2541"
---

# An authorization check in front of the handler reads with the handler's own parser, and says what licenses it to read early

A check mounted in one place in front of seventy routes is worth having, and it has three ways of
failing open that each read as correct code.

- **Resolve the target with the parser the handler will use.** Hand the raw path value to the
  same store the handler addresses its row by, and read that store's own malformed-input error as
  "resolves to nothing". A second parser in front of a handler that does not use it admits every
  spelling the two disagree on: Go's `uuid.Parse` refuses a hyphen after any group of four digits
  and Postgres accepts it, so a check that parsed ids in Go resolved nothing where the handler
  resolved a row, and a scoped caller's write reached a project the check had judged absent.
  `scanProjectKey` and `unaddressed` in `packages/envoy/internal/dispatch/api/project_scope.go`
  are the shape: every resolver passes `r.PathValue(...)`, and SQLSTATE `22P02` is read beside
  `pgx.ErrNoRows` as no target rather than as an error.
- **Prove, out of the tree, that nothing moves the object between the check's read and the
  handler's**, and write that proof in the file rather than in the pull request body, where the
  next reader will not find it. The licence `project_scope.go` records is that no
  `update ... set project_key` and no `update ... set issue_key` exists outside the migrations and
  the tests, so the check needs no transaction of its own. Without such a sentence a pre-handler
  read is an assumption about every write in the package, re-made silently at every merge.
- **Make the new check a provable no-op for every credential that exists today, and say so where
  it is implemented.** Then it deploys before anything presents the new credential, and deploy
  ordering leaves the risk list. `project_scope.go`'s header names each credential that carries no
  project — cookie, personal token, Kubernetes service-account token, shared token, and a
  broker token scoped to a person — and says it is the rule rather than a transitional measure.
- **Moving a check earlier changes which refusal a malformed input gets.** Record the new code in
  the handoff and the body: with the project check above `POST /api/v1/issues`'s external-ref
  branch, a scoped caller sending a malformed project key now gets `403 PROJECT_SCOPE` where it
  got `400 INVALID_ISSUE`. Nothing in a diff shows that.

Fail-closed is also a claim about what the refusals tell a caller. Write what is true: here the
two refusals read differently on purpose — another project is named, a target that resolved to
nothing is not — so failing closed closes no existence oracle, and the code, its test and both
guides say that rather than the opposite.

## Evidence

sjawhar/legion#2541 (INFRA-264) added `withinProjectScope` and `withinSessionScope` over every
mutating Dispatch route. Review round 1 found the uuid-spelling hole with
`a061-e3f2-c3a0-41f0-80b2-3339-e0cc-3a57`, which answered `200` where the canonical spelling
answered `403`; the fix moved every resolver onto the raw path value (`02b80896`). The claim that
failing closed "also closes an existence oracle" was corrected in the code and its test in round 2
and in `packages/envoy/AGENTS.md` and `cmd/dispatch/README.md` a round later (`98f089a9`) — a
prose copy of a corrected sentence costs a review round of its own.

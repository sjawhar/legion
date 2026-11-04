---
title: "An identifier sweep is bounded by what the token addresses, and closed by the literals its own diff removed"
category: best-practices
tags:
  - rename
  - sweep
  - fixtures
  - verification
date: 2026-11-04
status: active
module: repository-wide
related_issues:
  - "cybermonklabs/legion#2734"
---

# An identifier sweep is bounded by what the token addresses, and closed by the literals its own diff removed

Extends docs/solutions/testing/argv-format-change-is-a-fixture-population-sweep.md.

Renaming an owner, a package or a URL across a tree is two problems, and a single regular
expression answers neither.

## Bound the sweep by what each occurrence does, never by the path it is in

- Before the first rewrite, enumerate the classes of the old token that must **not** move, and make
  each class a property of the token: a module path, a package scope, an issue citation, another
  repository, a fixture value.
- Rewrite per occurrence by what the occurrence does. One a reader or a program uses to **reach**
  the thing moves. One that **records** something published under the old name — a citation, a past
  image tag, a package version id, a certificate SAN — keeps its identifier exactly and moves the
  owner into the prose around it, because rewriting it states a fact that was never true.
- A word-boundary lookahead cannot express "not a module path". `old/name(?![#\w-])` matches
  `github.com/old/name/module` as readily as a live address, since `/` ends no word.
- **Exclude no file from the sweep.** Excluding a class of files to protect one allowed occurrence
  in it protects every disallowed occurrence in it too, and the ones it hides are expectations.
- A derivation covers only what runs where the environment supplies the value. Where one script
  already derives every repository-dependent path from `$GITHUB_REPOSITORY`, a transfer costs that
  half one line — its off-runner default — and nothing else; Go constants, shell fixtures, runbook
  commands and prose hold literals by necessity, and for them a sweep is the only instrument.

## Close the sweep with the literals your own diff removed

A grep for the old token answers "is any left"; it does not answer "did anything that mirrors a
moved value stay behind". Take the completion set from the diff:

1. Extract from the diff every literal the **non-test** half removed, and keep those whose
   replacement the same diff adds.
2. Grep each across every file the sweep skipped — `*_test.go`, `*.test.sh`, `*.test.ts(x)`,
   `testdata/`, `fixtures/`, `e2e/`.
3. Read every survivor and name its class in the handoff.

Run that once, after the first stale expectation, not once per red lane. An acceptance criterion
written as a grep is a **floor** on the work: it proves the shapes it names are absent and says
nothing about a value the code now writes differently, so a spec states the completion set as
"every literal the code now writes differently" and keeps the grep as its cheap check.

Two corollaries:

- A fixture that must equal a production string derives it, or is wrong eventually. Where it cannot
  derive it, its comment names the production file it mirrors — and such a comment is a lie the
  moment the literal beside it is hand-edited.
- A derivation's own tests pin literals on **both** sides, so the test tree is part of the sweep,
  which is the opposite of the exclusion heuristic.
- Enumerate every directory holding a `go.mod` that the diff touched and run each module's own
  gate. A module outside the root `go.work` is reached by no root `go test` or `go vet`
  (docs/solutions/github/a-go-jobs-path-filter-is-the-closure-of-what-its-command-compiles-production-imports-included-and-stops-at-a-nested-module-INFRA-583.md).

## Evidence

cybermonklabs/legion#2734 moved this repository's own address from the `sjawhar` owner to
`cybermonklabs` after the GitHub transfer of 2026-11-03.

- The first pass used `sjawhar/legion(?![#\w-])`. It rewrote `github.com/sjawhar/legion/daemon` in
  218 files; `go build ./...` in `packages/daemon` refused, and the module path was put back.
- Classes that stayed, at `6a0d666e17d9`: the module paths in `packages/daemon/go.mod:1`,
  `packages/envoy/go.mod:1` and `packages/envoy/infra/go.mod:1`; the `@sjawhar` npm scope; the
  `sjawhar/legion#NNNN` citations; `sjawhar/legion-smoke`, another repository, with the pod, cache
  and session names derived from it; and the fixture owner `sjawhar` with its `ghs_sjawhar_read`
  fake tokens and `sjawharlegion` project token. `.github/e2e-scripts.json:109` holds both halves
  in one line: the prose moved to `cybermonklabs/legion`, the citation `sjawhar/legion#2512` did
  not.
- Four records keep their identifiers and name the owner in prose with the transfer date: a
  buildcache tag and two package version ids
  (`docs/solutions/github/a-branch-experiment-writes-only-state-scoped-to-the-branch-or-state-its-app-can-delete-pr2297.md:35`),
  two past image tags, and a recorded certificate SAN
  (`docs/solutions/github/keep-an-attestation-signer-identity-by-calling-one-reusable-workflow-build-only-then-attest-only-INFRA-576.md:50`).
- The sweep excluded `*_test.go`, `*.test.sh` and `*.test.ts` to protect the fixture owner. That
  hid three stale expectations, each found one red CI lane at a time across three rounds:
  `packages/envoy/infra/aws/webhooklistener_test.go:173` against the alarm description at
  `packages/envoy/infra/aws/rendering/metricfilters.go:149` (five tests red);
  `packages/envoy/loadtest/cluster/cluster.test.sh:555` and `:1053` against the call
  `cluster.sh:274`'s `LEGION_REPO` makes; and
  `packages/daemon/internal/runtime/sandbox/probe_test.go:184`, whose own comment calls
  `testManifest` "testImage's manifest URL" while the constant held the old path. **None of the
  three matches the acceptance grep**, whose four shapes were `ghcr.io/sjawhar`,
  `--repo sjawhar/legion`, `github.com/sjawhar/legion/(releases|.github)` and `sjawhar.github.io`.
- The diff-literal extraction found 119 distinct removed tokens from the non-test half. Grepped
  across every test and fixture file, it reached the two remaining expectations and nothing else;
  the tester's independent extraction of 140 tokens left fourteen shapes, each in an allowed class.
- `packages/envoy/infra` has its own `go.mod` and is not bound by the root `go.work`, so the
  round-1 gates (`go vet ./...` at the root, every root test run) passed over the first of the
  three.
- `.github/scripts/repo-paths.test.sh` pins the literal table on both sides, and `ci.yaml:3309-3312`
  sets `GITHUB_REPOSITORY` deliberately and asserts a full literal URL: the clearest counterexample
  to excluding tests from the sweep.

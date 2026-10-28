---
title: "A committed corpus differential compiles at base, dumps deterministically, and each surface gets a negative control its input can trigger"
category: testing
tags:
  - differential
  - byte-identity
  - corpus
  - negative-control
  - git-archive
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/docs
related_issues:
  - "sjawhar/legion#2451"
---

# A committed corpus differential compiles at base, dumps deterministically, and each surface gets a negative control its input can trigger

Extends docs/solutions/testing/a-no-op-refactors-oracle-is-the-fork-point-binarys-output-diffed-in-the-worker-pod-and-a-derived-set-is-asserted-exactly-LEGION-636.md.
This page covers a byte-identity claim checked over a large corpus, where the oracle is the base's
own code run by one test file at both revisions.

- Commit the differential as a test that skips unless an environment variable names the corpus.
  Copy the same file into a `git archive` export of the base and run it there too. Reach the code
  only through an entry point both revisions have (here `buildDocumentRead`), never through the
  functions the change rewrites, or the file does not compile at base.
- Make the dump the same on every run of one revision:
  - rename ids the code mints at random to ids counted in pre-order (`b-<n>`);
  - turn a refusal or a panic into its own dump line, so the two dumps stay aligned line for line;
  - log timings and never dump them;
  - name fields a struct tags `json:"-"`, since `json.Marshal` drops them from the dump with no
    error.
  Run base twice before you trust a `cmp` of base and head.
- Add the inputs the corpus lacks to the corpus itself. Re-parsed markdown carries no marks, so the
  test adds anchor marks to every fifth text node.
- Pick each surface's negative control from what that surface's input contains. A mutant that
  breaks a case the input never reaches proves nothing: the HTML-escape mutant changed 851 of
  1,200 LINUX dump lines and none of the 2,000 API answers on the load-test seed, which holds no
  `<`, `>` or `&`. Before you claim a control, confirm that it changes the output.

## Evidence

- `packages/envoy/internal/dispatch/docs/token_corpus_test.go` (commit 52f28b5a): its header
  comment states the base-compatibility rule; `tokenCorpusDoc` renames ids and adds marks;
  `tokenCorpusBlock` names `ArtifactBlock.DescendantIDs` (`json:"-"` at `model/model.go:637`);
  each step's panic becomes the line's `error`.
- The implementer's dumps at base 59f6647d and head 0483541b8cbe were `cmp` identical (94,779,048
  bytes), and so were two base runs. The API comparison used a second mutant, block spans that
  start one byte late, which changed all 1,000 `/blocks` answers (PR body, `E2E (implementer)`).

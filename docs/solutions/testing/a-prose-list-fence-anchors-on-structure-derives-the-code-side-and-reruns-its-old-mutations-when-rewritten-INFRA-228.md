---
title: "A prose-list fence anchors on structure, parses by shape, derives the code side, and re-runs its old mutations when rewritten"
category: testing
tags:
  - documentation-fence
  - operator-docs
  - mutation-testing
  - go-test
date: 2026-10-29
status: active
module: docs
related_issues:
  - "INFRA-228"
  - "sjawhar/legion#2503"
---

# A prose-list fence anchors on structure, parses by shape, derives the code side, and re-runs its old mutations when rewritten

Extends docs/solutions/legion/an-operator-facing-sentence-outlives-what-it-points-at-INFRA-31.md.

That document says to fence a list in prose with a test. These rules decide whether the fence can
fail:

1. **Anchor on a structural line**, such as the section heading matched as a whole line. A
   hard-wrapped sentence needs whitespace flattening to find, and it is text a later edit rewords
   for good reasons.
2. **Parse members by the list's shape**: the backticked name a bullet opens with, followed by its
   separator. Never accept "the name appears somewhere in the section". A neighbouring bullet's
   prose can name the member you deleted.
3. **Derive the code side from the code that decides it.** Call the production function once per
   branch it can take and take the union of what it writes. A list typed into the test passes when
   someone adds a member to the code.
4. **Compare both sides sorted with `slices.Equal`**, so a member listed twice fails too.
5. **Fail on every empty result**: no anchor line, no bullets, a decision that wrote nothing. A
   skip or an empty set reads exactly like a pass.
6. **Make CI select it.** A test that reads a file outside its module needs that file in its job's
   filter and a `RULES` entry in `.github/scripts/ci_job_reads.py`
   (`docs/solutions/github/join-a-job-to-the-ci-router.md`).
7. **When a review round rewrites how the fence finds or derives what it checks, re-run every
   mutation the previous version caught.** For each one that now passes, either restore the check
   or put the gap in the pull request body's *Not proven / risk*. Change the failure message so it
   no longer names what the test stopped reading.

## Evidence

`packages/envoy/internal/broker/oauth/claims_doc_test.go` (sjawhar/legion#2503) holds the claim
list in `docs/kubernetes.md` to the broker's exchange. Its first version anchored on a wrapped
sentence, used a hand-typed list of the four `Claim*` constants, and was not in CI's `envoy_go`
filter. Review round 1 (`.legion/INFRA-228/review.json`, `roundOneFindings`) asked for each fix,
and `bcb045fa` made them: the heading anchor `claimsDocHeading`, `issuedClaims` calling `decide` for
a pod token and a machine token, `slices.Equal` on sorted slices, and the `ci.yaml` and
`ci_job_reads.py` entries. A deleted `project` bullet fails even though the `project_scope`
bullet's prose names `project`.

The tester then found that the heading anchor lost one mutation the sentence anchor had caught:
rewording the sentence "An issued token carries these claims and no others" now passes
(`.legion/INFRA-228/test.json`, `observations[0]`). The failure message still quoted that sentence
until the reviewer's `f56d98ac` changed it. The pull request body records the gap under
*Not proven / risk*.

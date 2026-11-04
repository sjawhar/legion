---
title: "A forward merge is a second way two copies of one rule appear"
category: envoy
tags:
  - refactor
  - duplication
  - oracle-test
  - merge
date: 2026-11-04
status: active
module: packages/envoy/internal/dispatch/refs
related_issues:
  - "sjawhar/legion#2589"
---

# A forward merge is a second way two copies of one rule appear

Extends docs/solutions/envoy/a-batch-version-beside-a-kept-single-key-function-makes-that-function-its-one-key-call-pr2447.md.
That one is about the copy a refactor leaves behind;
this one is about the copy a merge brings in, and about what makes the oracle that holds the single copy honest.

- Sweep for a duplicated rule after every forward merge of the base, not only after a refactor of your own:
a long-lived branch and the base can each move the same rule, and the merge joins both texts with no conflict.

- Keep one copy of the rule and have the single-key reader call it with one key,
so the batch reader and the single reader can never answer differently for the same input.

- Freeze the oracle as the literal text the change replaced, with its constants spliced as the numbers that text held,
never built from the package's own constants: an oracle built from them moves with them, and the comparison passes however the rule changed.

- Prove the oracle by breaking the rule and quoting the failure it produced,
then put the rule back.

## Evidence

In sjawhar/legion#2589 the repository-resolution rule is one text, `IssueRepoTerms`,
described as "the server's one repository-resolution rule.
The issue page resolves a page of issues through these terms (api's issue reads) and IssueRepository resolves one key through them,
so the currency mark and the pull-request head attribution can never disagree with the issue page"
(`packages/envoy/internal/dispatch/refs/issues.go:96-121`).
The single-key reader is that same text over a one-key `subjects` term,
`with recursive subjects as (select $1::text as key)`
(`packages/envoy/internal/dispatch/refs/issues.go:127-135`),
read by `IssueRepository`
(`packages/envoy/internal/dispatch/refs/issues.go:137-143`).

The duplicate arrived through a forward merge:
the pull request body records that "the forward merge left two production copies of the issue repository rule,
so the terms, the join and the columns moved into `refs` beside the depth cap, `refs.IssueRepository` runs them over a `subjects` term of one key, `IssueRepoLateral` is deleted",
fixed in commit `0f5916f0`, "one copy of the issue repository rule, in refs, for both of its readers".

The oracle is frozen as literal text:
"mainIssueRepoLateral is main's own SQL, held here letter for letter as the oracle of the comparison below.
It is a copy of the text this change replaced, not text built from the package's constants:
a lateral built from those would move with them, and the two sides would agree however the resolution rule changed.
IssueDepthCap is written as the 32 main spliced into it for the same reason"
(`packages/envoy/internal/dispatch/refs/issues_test.go:135-140`).
The comparison is `TestIssueRepositoryAnswersWhatMainsLateralDid`
(`packages/envoy/internal/dispatch/refs/issues_test.go:263`),
over a loop, a chain at the cap, a chain past it, an issue naming its own repository, an issue inheriting one, a project mapping no default, and a key no issue has.

The oracle was proven by breaking the rule:
the pull request body records that with `IssueDepthCap` at 64 the equality test failed with
`issues_test.go:282: "CORE-34", the issue one link past the cap: the terms answered acme/widgets, main's lateral refused`,
"and the constant was put back".

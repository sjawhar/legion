---
title: "An instrument that answers a smaller question returns the comfortable answer"
category: best-practices
tags:
  - search-method
  - github-api
  - review-round
  - documentation-drift
  - evidence
date: 2026-10-25
status: active
module: packages/envoy
applies_when:
  - You are about to state a fact about every file, commit, run or review of a pull request
  - A count you print lands on 100, 300 or 1,000
  - You are checking a sentence that joins two things with "and"
  - You are citing code by line number in a document, a comment or a review
related_issues:
  - "INFRA-141"
  - "sjawhar/legion#2167"
---

# An instrument that answers a smaller question returns the comfortable answer

Extends docs/solutions/best-practices/a-completeness-claim-from-a-search-is-only-as-good-as-its-vocabulary-its-window-and-its-order-INFRA-250.md.

That rule governs a search you run. This one governs the **tools you read the answer out of**, and
the claims that survive a reading. Each instrument below silently answers a narrower question than
the one asked, and in every case the narrow answer is the one that lets the work proceed.

## Rules

- **Distrust a count that lands on a round number.** 100, 300 and 1,000 are page and cap sizes,
  not facts about your change. `gh pr view --json files` returns 100 files; the compare endpoint
  caps at 300 **and may omit its `files` key entirely**, so a predicate over that list answers
  `true` for a diff it never saw; a workflow run list stops at 1,000; `gh api .../reviews` pages,
  so a default read can show neither approval. Read the cap's own marker — `truncated`,
  `total_commits`, `files_returned` — or page explicitly, and say which you did.
- **A hash of nothing is a hash.** `e3b0c442…` is the sha256 of empty input, so a pipeline whose
  read failed hashes to a value that compares equal to another failed read. Fail the line
  (`set -o pipefail`) and assert the input is non-empty before you compare digests; a digest
  comparison with no size check proves two commands failed the same way.
- **A sentence joined by "and" is two claims; check each conjunct against its own call path.**
  "The role-holder read **and** the publish take the same deadline" was true of the publish and
  merely asserted of the read, and read as a whole it passed ten review rounds, two audit passes
  per round, and a reviewer that had caught four sentences of the same class. The conjunction is
  what hid it: the true half carries the false one.
- **A deadline claim is decidable from a signature.** A function that takes no `context.Context`
  cannot observe a caller's deadline. Before writing that a path is bounded by a caller's context,
  read the declaration of every function between the context and the work; that check costs one
  grep and does not depend on reasoning about the body.
- **Cite code by symbol, not by line.** A line number is a citation that goes stale whenever
  anything above it moves, and a stale line citation reads as precise, so it survives review. A
  risk entry citing `api.go:910` was made wrong by a comment edit eleven lines above it; the entry
  now names the calling symbol, which cannot drift. Keep line numbers for one-off observations
  (a review comment, a message), never in a document or a code comment meant to last.

## Evidence

All five instruments and both claim defects come from INFRA-141 (sjawhar/legion#2167) on
2026-10-25, a change with eleven implementing rounds.

- `gh pr view 2167 --json files` returned exactly 100 entries for a 33-file pull request plus its
  history; the number is the cap, and nothing in the output says so.
- The compare endpoint answered `ahead_by: 1` with a one-entry `files` list; a run on another head
  returned no `files` key at all, where a bare `all(...)` over the missing list would have answered
  `true` — a clean verdict about a diff that was never read.
- A review read without `--paginate` showed neither of two approvals, because both had paged out.
- A proposed proof that two heads compiled identically compared `sha256` of comment-stripped
  source; the command that produced one side could fail and still print `e3b0c442…`, which would
  have matched a second failure exactly.
- The conjunction sentence (`cmd/listener/api.go:848-850` as it then stood) claimed the caller's
  deadline governed both the role-holder read and the publish. `resolveLiveRoleHolder` takes no
  context parameter at all and reads under `context.Background()` with a per-attempt bound, so a
  role-lane publish can exceed the budget before the bounded publish begins. A fresh reader with no
  prior, reading the route end to end rather than reading diffs, found it in one pass after ten
  rounds had not.
- The stale citation was in the reviewer's own risk entry and was made wrong by the commit that
  fixed the conjunction.

---
title: "A forward merge after retro falsifies the first retro's own citations: the second retro sweeps every artifact the round wrote"
category: legion
tags:
  - retro
  - forward-merge
  - learnings
  - citations
date: 2026-10-30
status: active
module: docs/solutions
related_issues:
  - "sjawhar/legion#2542"
---

Extends `docs/solutions/legion/a-forward-merge-after-retro-reinstalls-when-main-bumped-the-lockfile-keeps-both-changelog-insertions-and-still-runs-test-review-and-a-second-retro-LEGION-634.md`,
which says what the second retro costs, and
`docs/solutions/legion/correct-a-falsified-clause-of-another-retros-learning-in-place-and-split-the-tense-pr2545.md`,
which authorises the in-place correction. The delta is who sweeps, and what.

## Rules

- **The learnings a retro writes are branch artifacts, and a later forward merge moves what they
  cite.** A retro that lands before a conflict round cites the branch's code at that head; the
  merge then brings main's commits through the same files and every line number in those pages is
  a guess. The second retro's first step is the sweep, before it writes anything new.
- **The sweep is one command over everything the round authored** — the learning pages, the
  handoff, and the pull request body:
  `grep -rno "[a-z_/]*\.\(go\|md\|ts\):[0-9]\+\(-[0-9]\+\)\?" docs/solutions/*/*pr<number>.md`.
  Check each hit against the merged tree and repoint it at the symbol, which is the standing rule
  in
  `docs/solutions/best-practices/an-instrument-that-answers-a-smaller-question-returns-the-comfortable-answer-INFRA-141.md`
  ("cite code by symbol, not by line"); the merge is simply the event that makes a line citation
  wrong fastest.
- **The never-edit rule binds concurrent retros, not your own round.** Correcting a citation your
  own round falsified is the in-place edit pr2542's reviewer and pr2545 both make; it is not a
  second retro editing a stranger's page.
- **Cite another `docs/solutions/` page by its path and the name of its rule**, never by line:
  those pages are edited in place by exactly this sweep.

## Evidence

sjawhar/legion#2542's first retro wrote five learnings at `4f51f06f`. Main then moved 19 commits,
the head conflicted, and the forward merge `c23af746` brought sjawhar/legion#2504 through
`internal/bus/nats.go`, `internal/bus/recovery.go` and `cmd/listener/main.go`. Four citations were
wrong at the merge head: `nats.go:529`, `:414`, `:523-528` and `recovery.go:198-215` in the
`bind-a-wait-…` page, which the reviewer repointed at symbols in `9eddf894b532`, and
`cmd/listener/main.go:608-614` in the `prove-a-dial-option-…` page, which at that head is
`openWebhooks` and not `listenerStreamOptions` (now at `:645`). The second retro's sweep found the
last one, with two more of the same class — `connect.go:72-73` for `bus.WithSilentCutDetection`,
whose body is `detectSilentCuts` while the option itself is in `nats.go`, and
`silent_cut_test.go:68-76` for a failure message that begins below that range — and repointed all
three at symbols.

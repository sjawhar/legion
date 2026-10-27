---
title: "A trailered head is missing the checks a push starts, not every check run: diagnose it by the base's required set, never by counting the head's runs"
category: legion
tags:
  - skip-checks
  - required-checks
  - ci
  - merge-gate
  - diagnosis
date: 2026-10-24
status: active
module: legion-worker
related_issues:
  - "INFRA-149"
  - "sjawhar/legion#2143"
---

# A trailered head is missing the checks a push starts, not every check run

Extends docs/solutions/legion/a-handoff-only-push-runs-no-check-so-check-private-names-over-a-handoff-fires-only-at-the-next-code-push-LEGION-698.md.

That document is right that a push changing only `.legion/` starts no workflow. The delta is what
you may conclude from the head afterwards.

- **`skip-checks: true` suppresses the runs a push starts, and nothing else.** A workflow that
  triggers on something other than the push still runs and still attaches its check run to that
  head. In this repository `.github/workflows/pr-public-text.yaml` triggers on
  `pull_request_review`, so any review (an inline comment posted alone included) puts a
  `public-text-scan` check run and a `public-text` commit status on a head no push ever built.
  Neither is required; until 2026-10-27 `pr-title.yaml` also ran on reviews and put the required
  `pr-title` there.
- **So "the head has no check runs" is the wrong diagnostic.** It is wrong in exactly the case you
  will meet — a blocked pull request someone has reviewed — and a reader who finds runs there
  concludes CI is merely slow and waits.
- **Ask instead whether the checks the base requires *from a push* are present.** Read the base's
  required set live (`gh api repos/<o>/<r>/rules/branches/<base>`, plus
  `.protection.required_status_checks` when it has any), then read the head's check runs and name
  the missing ones. A head missing some but not all of the required set is the signature of a
  trailer, not of a slow queue.
- **Write a measurement's controlled conditions next to its number.** A paired measurement of the
  trailer that forbids commenting between the push and the read will show zero runs; that zero is
  the condition, not a general truth, and quoting it bare invites the next reader to think the
  measurement was sloppy when they meet a trailered head with runs on it.

Avoid the formulation in
docs/solutions/legion/a-base-with-no-required-check-makes-the-ci-verdict-a-hand-read-against-a-branch-line-baseline-INFRA-117.md
("a handoff-only head ... shows zero"): true only while nothing has commented.

## Evidence

sjawhar/legion#2121 sat `MERGEABLE`/`BLOCKED` at head `917c49e9`. Read on 2026-10-24:
`gh api repos/sjawhar/legion/commits/917c49e9 --jq '[.files[].filename]'` returns exactly
`[".legion/INFRA-83/review.json"]`, its commit message ends with `skip-checks: true`, and
`gh api repos/sjawhar/legion/commits/917c49e9/check-runs` returns two completed, successful
`pr-title` runs and nothing else. `lint`, `typecheck` and `test` — the other three main requires —
are absent. Counting runs says "two checks, both green"; counting the required set says "three of
four never ran", which is the fact.

The paired control, from the same issue: on scratch pull request sjawhar/legion#2132, head
`9f9b8346` (one file deleted, no trailer) carried all four required checks, completed and
successful, while head `0af01af0` (the same shape with the trailer as the last line after two
empty lines) carried `total_count: 0` and no workflow run at all. That zero held because the
measurement forbade any comment, review or body edit on the pull request between the push and the
read, for this document's first reason.

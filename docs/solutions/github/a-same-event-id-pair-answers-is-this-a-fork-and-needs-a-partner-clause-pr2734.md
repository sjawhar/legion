---
title: "A same-event id pair answers \"is this a fork\", and needs a partner clause for the events that carry no pull request"
category: github
tags:
  - github-actions
  - fork-gate
  - rename
date: 2026-11-04
status: active
module: .github
supersedes: docs/solutions/github/a-workflow-asks-whether-a-run-is-on-its-own-repository-by-id-never-by-name-pr2734.md
related_issues:
  - "cybermonklabs/legion#2734"
  - "cybermonklabs/legion#2740"
---

# A same-event id pair answers "is this a fork", and needs a partner clause for the events that carry no pull request

Corrects the operand and the citations of
`docs/solutions/github/a-workflow-asks-whether-a-run-is-on-its-own-repository-by-id-never-by-name-pr2734.md`,
which told the next worker to compare `head.repo.id` with `github.repository_id`. The tree does
not do that in any of its four places, and the form it does use is the better one. What that page
says about ids rather than names, and about such a fix being unprovable before the merge, stands.

## The rule

- Compare the head repository's id with the **base repository's id, both read from the same
  event**: `github.event.pull_request.head.repo.id == github.event.pull_request.base.repo.id`.
  Never a payload field against a context value, and never a name against a name.
- The reason is not only that an id survives a rename. One event is **one epoch**: a re-run
  executes the workflow files of its own commit against the first attempt's payload, so a
  comparison whose two operands come from that payload is self-consistent whatever happened to the
  repository since. A payload field against `github.repository_id` mixes an epoch, and is saved
  only by the id's stability — an argument the same-event form never has to make.
- **Give it a partner clause on any workflow that also runs outside a pull request.** On `push` and
  on `merge_group` there is no `pull_request` in the payload, both operands are absent, and GitHub
  compares them equal: the gate opens. That is harmless only because something else decides those
  events. Where the step should report on them, pair it with
  `github.event_name != 'pull_request' ||`; where it should not, pair it with a condition that is
  false there.
- Cite such a condition by the symbol that holds it, never by a line number: four of these moved
  twice in one day.

## Two warnings about landing a fix of this class

- **A repository-wide CI defect is visible to every tree**, so fixing it inside a pull request
  about something else races all of them. The loser of that race pays a forward merge, a body
  census and a review round for a change it no longer carries. Report the defect and let it land
  on its own, or expect to drop your copy.
- When main lands the same fix first, take main's lines whole and drop yours
  (`docs/solutions/legion/a-forward-merge-onto-mains-competing-rule-keeps-mains-code-and-deletes-your-copy-and-its-tests-pr2366.md`).

## Evidence

- At `56dc9ac4da44`, `.github/actions/ci-ok-report/action.yml`'s three steps and
  `.github/workflows/ci.yaml`'s "Post pr-title on the head" step all read
  `github.event.pull_request.head.repo.id == github.event.pull_request.base.repo.id`. The action's
  three carry `github.event_name != 'pull_request' ||` before it, so a `push` or `merge_group` run
  reports; `ci.yaml`'s carries `steps.pr-text.outcome != 'skipped'`, and the step it names is
  itself `if: github.event_name == 'pull_request'`, so the absent-field equality is never reached
  there.
- cybermonklabs/legion#2740 (`e264a671`, merged 2026-11-04T04:15:11Z) made that change about four
  hours after #2734's `d63ae736` made the `github.repository_id` one in the same two files. #2734's
  second forward merge took main's four lines whole and dropped its own, with the eight-line
  description block that explained them; `action.yml` then left #2734's file set entirely, and its
  changed-file count went 239 → 229.
- The defect both fixes answer: the repository was transferred from `sjawhar` to `cybermonklabs` on
  2026-11-03, and a re-run of run `41668010626` afterwards posted no `ci-ok`, because the payload's
  name and `github.repository` no longer agreed.
- The superseded page's own citations are already stale, which is why this one cites by symbol: it
  named `action.yml` `:33`, `:40`, `:49` and a description block at `:11-16`, where the steps are
  now `:32`, `:39`, `:48` and the block at `:10-16` is main's, citing run `41668010626`.

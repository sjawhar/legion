---
title: "Merge-queue required checks need a `merge_group` trigger on the workflow that produces them: `checks_requested` is the only activity type, `push.branches: [main]` never matches a queue ref, the trigger lands before the rule, and the queue run itself is provable only after the rule"
category: github
tags:
  - merge-queue
  - merge_group
  - github-actions
  - required-checks
  - ruleset
  - actionlint
  - ci
date: 2026-09-14
status: active
module: ci
related_issues:
  - "LEGION-99"
  - "sjawhar/legion#1089"
  - "LEGION-97"
  - "LEGION-90"
  - "sjawhar/legion#1059"
symptoms:
  - "two pull requests each green on their own merge a minute apart and main is red on typecheck (LEGION-97: #1007 and #1030, main red until #1045; #1046 and #1035 then failed in a file they never touched)"
  - "a merge queue rule is on but every queued pull request times out: the required checks never start on the merge group"
  - "actionlint: unknown Webhook event \"merge_grup\""
---

# Merge-queue required checks need a `merge_group` trigger on the workflow that produces them

GitHub's merge queue builds a temporary merge commit of each queued pull request on top of the
current `main` (and everything queued ahead of it), runs the branch ruleset's **required checks by
name** on that commit, and merges only if they pass. That closes the LEGION-97 gap — two pull
requests each green against an older `main` cannot land together and break it — without forcing a
rebase or a new commit on anyone's branch (Sami, 2026-09-11: "Please don't do unnecessary
rebases"). But the queue does not run anything by itself: it waits for check runs with the required
names to appear on its commit. What LEGION-99 (sjawhar/legion#1089) established about producing
them:

## 1. `push.branches: [main]` never fires on a queue commit; add `merge_group:` to the same workflow

The queue's temporary commit lives on `refs/heads/gh-readonly-queue/main/pr-<n>-<sha>`, so a
workflow scoped `on: push: branches: [main]` does not run there, and `pull_request` does not fire
either (the commit is not a pull request head). The trigger is its own event:

```yaml
on:
  push:
    branches:
      - main
  merge_group:
    types: [checks_requested]
  pull_request:
    types: [opened, reopened, synchronize]
```

Put it on the **workflow that already emits the required job names** (`lint`, `typecheck`, `test`
in `.github/workflows/pr-and-main.yaml`). The ruleset looks checks up by job name regardless of
which event produced the run, so the existing jobs gate the queue with no `if:`, no
`concurrency:`, no second workflow. Do not add an `if:` to a required job that could evaluate false
under `merge_group`: a required check that never appears fails the group at the ruleset's
status-check timeout, and every pull request in that group is removed.

`checks_requested` is the **only** workflow activity type for `merge_group` (GitHub's own example
is `types: [checks_requested]`); `destroyed` is a webhook action only and never needs a run. Naming
the type keeps the trigger greppable — `yq '.on.merge_group.types'` — and is what actionlint
validates.

## 2. Jobs that read the pull request stay out of the merge group

`github.event.pull_request` is absent on a `merge_group` event. The PR-title check
(`.github/workflows/pr-title.yaml`, moved out of `Tests` by LEGION-90 / #1059 because it alone
needs the `edited` event) is therefore correctly `on: pull_request` only: a merge group has no
title to check, and a merge-group payload never carries `edited`. Do not fold it back into `Tests`
under "one workflow for the queue" pressure — re-derive that constraint first. A required check
that is pull-request-only must not be in the ruleset's required list, or the queue waits for it
forever.

## 3. The trigger lands on `main` before the rule is switched on — never the other way round

Turning the merge queue rule on before the workflow answers `merge_group` leaves every queued pull
request waiting for checks that never start, and the group fails at its timeout. The change has
three ordered steps: (1) the trigger merges into `main` (this is an ordinary pull request); (2) an
administrator adds the merge queue rule to the `main` ruleset (a repository setting, not code — on
`sjawhar/legion` it is Sami's click, requested as one plain `dispatch_message` carrying the exact
settings, never an ask); (3) the merge-queue organizer's `gh pr merge --squash --match-head-commit
<sha>` becomes an enqueue on its own, since GitHub routes the same command into the queue once the
branch requires one. GitHub builds each queued pull request's merge commit **with the workflow
files as merged**, so the trigger's own pull request would pass through the queue either way; it is
every *other* pull request that would stall. The rollback is deleting the one rule; the trigger is
inert without it.

## 4. What can be proven before the rule exists, and what cannot

Before the rule, no `merge_group` run exists anywhere, so the PR's proof is the trigger block
itself, on the surfaces that actually parse it:

- **GitHub's own parse.** The pull request's `Tests` run at the head (`legion gh -- run view <id>
  --json jobs`): GitHub rejects an invalid trigger block with a failed "workflow file issue" run
  instead of running the jobs, so a green run under `pull_request` proves the file with the new
  block was accepted.
- **actionlint on the file** (`actionlint .github/workflows/pr-and-main.yaml` → no findings, exit
  0), with the negative control `sed 's/merge_group:/merge_grup:/'` → `unknown Webhook event
  "merge_grup"`, exit 1. actionlint is the one local check that knows the event vocabulary.
- **A structural read** of the trigger keys, the activity type, and every required job's `if:`
  (`yq`, or `js-yaml <file> | jq` where `yq` is missing — see
  [worker-pane-shell-gotchas](../legion/worker-pane-shell-gotchas.md) §17).

The merge-group run itself, and the acceptance lines that depend on it (a pull request green
against an older `main` is held when combined with the current one; an ordinary one lands with its
branch untouched; `main` green at every merge commit), are provable **only after step 3** and are
the implementer's production verification after the switch. Write that boundary into the PR body's
`E2E` lines in so many words — "the merge_group run is observable only after the ruleset change;
acceptance 1–3 are the implementer's production verification per the spec's Testing section" — so
the reviewer and merger read it as a declared scope, not as an under-tested change. A spec that
orders the switch after the merge (as LEGION-99's did, controller-ruled) makes that the correct
shape; a spec that does not is the place to fix.

## 5. Side effects of the queue that other consumers see

- Each queue build pushes to a `gh-readonly-queue/main/pr-<n>-<sha>` ref, which Envoy publishes as
  an ordinary branch push (`notifications.github.<owner>.<repo>.push.branch.<sanitized ref>`); the
  Legion daemon ignores a push on a branch with no registered pull request, so nothing changes
  there.
- GitHub sends the `merge_group` webhook event (`checks_requested`, then `destroyed`) to every App
  subscribed to it. Envoy's GitHub webhook must acknowledge it with 200 and publish nothing — see
  [an-unknown-github-event-publishes-an-empty-envelope](../envoy/an-unknown-github-event-publishes-an-empty-envelope-skip-it-by-name-after-signature-verification.md)
  for why the pre-LEGION-99 handler published a junk envelope instead.
- Check runs GitHub reports on the merge-group commit belong to no pull request head; Envoy's checks
  tracker records only check runs that list a pull request and, even then, drops any whose SHA is
  not the recorded head, so no `pr.<n>.checks` settlement is ever produced from a queue commit.

Related: [template-names-a-ci-check-the-repository-never-produces](../legion/template-names-a-ci-check-the-repository-never-produces.md)
(the ruleset's required names must be names the repository's workflows actually emit).

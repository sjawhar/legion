---
title: "A PR body's Fast-follow line becomes one same-issue PR after the parent merges: how it is scoped, branched, registered with the merge queue at open, and ended"
category: legion
tags:
  - legion
  - fast-follow
  - merge-queue
  - pr-queue
  - jj
  - implementer
  - reviewer
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-34"
  - "sjawhar/legion#1003"
  - "sjawhar/legion#1006"
symptoms:
  - "a PR body's `Fast-follow:` line names cleanup and nothing says who turns it into a change, when, or on which branch"
  - "the merge queue organizer learns a PR's scope and tier only from READY, after test and review are already spent on it"
  - "a follow-up PR's own review finds more tidiness and a worker reaches for another follow-up"
---

# A PR body's Fast-follow line becomes one same-issue PR after the parent merges

## Context

The `legion-worker` skill has every implementer write a `Fast-follow:` line — naming,
duplication, or wording findings batched into one item instead of iterating per push — and
says nothing about how that item becomes a change. LEGION-34 (sjawhar/legion#1003) carried four
such notes from its first review; the architect sent the implementer back after the squash
merge to land them as sjawhar/legion#1006, which the merge queue's organizer had ruled
non-major and allowed as its own PR. What follows is the procedure that round settled.

## Scope: exactly the parent body's line, no behaviour change

The follow-up's diff is the `Fast-follow:` line of the parent PR body, as written, and nothing
beyond it. On #1006 that was `GhCommandDeps extends GrantRedemptionDeps`, a `payload.data == null`
guard, the policy loop and flag parsers moved beside the rule they apply with unused exports made
module-private, and two unit pins — all on `packages/daemon/src/cli/`. Output lines, error
strings, exit codes, and query text stayed byte-identical and every existing test stayed green;
the PR body says so and the reviewer's thermo pass checks it (on #1006: "textually identical to the
pre-move body except `deps.log` → `log`"). A helper the notes did not name stays where it is even
when moving it would be tidy — `redeemGitHubToken` stayed in `index.ts` because moving it drags
`daemonUrl`/`grantFrom`/`readSecretPointer` along, and that is scope the organizer did not rule on.
Write the tests first: a unit pin that is green before the refactor and after it is the proof the
refactor changed nothing.

## Branch: `jj new main@origin` after the parent squash, never the issue bookmark

Open the follow-up only after the parent has merged, from the merged `main`:

```bash
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && \
  jj -R "$LEGION_WORKSPACE" new main@origin
```

Not on the issue bookmark `legion/<KEY>`: after the merge it sits on the retro commit plus an
undescribed working-copy commit that carries the daemon-provisioned, untracked `.omp/config.yml`
([text-only-skill-pr-mechanics](text-only-skill-pr-mechanics.md) §1), and the architect's
instruction is to leave it alone. `jj new` on a fresh parent removes that file from disk — copy it
aside and restore it, or the pane's extension provisioning is gone until relaunch. One path-scoped
`jj split` for the code, a new bookmark named for the issue and the slug
(`legion/<KEY>-<slug>`, here `legion/LEGION-34-cli-tidy`), `jj git push --bookmark <it>` (the
first push tracks it), then `legion gh -- pr create` with a conventional title that names the parent
issue (`refactor(cli): … (LEGION-34 fast-follow)`), `Dispatch: <KEY>` in the body, a link to the
parent PR, and the READY-format Verification block — `Threads: none`, `Thermo` and `E2E` left for
the reviewer and tester, `Fast-follow: none`, `Chain: not stacked`. The handoff file goes on this
new branch; the phase lifecycle (tester, reviewer, `.legion/` deletion, approval by SHA, retro,
merger) runs on it exactly as on the parent.

## Registering with the merge queue at open

The organizer of `notifications.role.pr-queue` wants to know about a PR opened outside the
one-PR-per-issue flow before READY, not from READY. Asked what "register with the queue at open"
requires (LEGION-34, 2026-09-13), the organizer's answer, verbatim in substance: **one message —
`envoy_send` to the organizer's session, or `envoy_publish` to `notifications.role.pr-queue`, both
reach it — carrying `repo#number`, the head sha, a one-line scope, the proposed tier (T4 for a
refactor with no behaviour change), and what it conflicts with** (name the directory and say
"nothing open touches it" when that is so). READY later carries the full packet — CI at head,
threads, pair, proof — as the merger role already states. The organizer arms a watcher on the head
from that message, so send a one-line follow-up when the head moves for a reason other than code
(a handoff commit): `#1006 head moved 0b75bf14 → d703e534 (handoff commit only; code head
unchanged; still T4, no conflicts)`. If a repository skill states an at-open form, use it; none
did on this repository, so ask the organizer once, non-blocking, and state the default you will
use if no answer arrives before the PR exists.

## A fast-follow has no fast-follow

The follow-up PR's own review records whatever tidiness it still finds as notes in the review body
— "recorded for whoever next touches these lines" — and its `Fast-follow:` line stays `none`. The
reviewer on #1006 put it as "a fast-follow to a fast-follow is not a thing": the field exists so
cosmetic findings stop a correctness round from iterating, not so cleanup PRs beget cleanup PRs.
Whoever next changes those lines for a real reason picks the notes up then.

## CI line on a narrow diff

A path-filtered workflow does not run on a diff outside its paths (`Legion Envoy and Contracts`
is filtered to `packages/envoy`/`packages/contracts` and never fired on #1006), so the template's
second run id is legitimately absent — say so in the `CI:` line rather than templating a
placeholder (see [text-only-skill-pr-mechanics](text-only-skill-pr-mechanics.md) §6 and
[pull-request-trigger-paths-follow-the-pr-head](../github/pull-request-trigger-paths-follow-the-pr-head.md)).
A `CI:` line that reads `run undefined` because the template assumed a second workflow is a wrong
fact, and correcting it is worth the extra CI run a body edit costs.

## Related

- [worker-pane-shell-gotchas](worker-pane-shell-gotchas.md) §13 — the pane's `legion` is the
  deployed build; the follow-up's live proof also uses `bun packages/daemon/src/cli/index.ts …`.
- [conflict-only-rebases-keep-the-diff-auditable](conflict-only-rebases-keep-the-diff-auditable.md)
  — the same never-rebase-without-a-conflict rule applies to the follow-up branch.

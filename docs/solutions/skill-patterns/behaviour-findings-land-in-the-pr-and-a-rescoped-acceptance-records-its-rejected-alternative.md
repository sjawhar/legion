---
title: "A review finding that changes behaviour lands in the PR, however small; wording and docs/solutions go to Fast-follow or retro; an acceptance line re-scoped on evidence records the rejected alternative so it is not re-proposed"
category: skill-patterns
tags:
  - review-round
  - fast-follow
  - no-deferrals
  - spec
  - acceptance-criteria
  - rejected
  - corrective-round
  - fixtures
date: 2026-09-13
status: active
module: legion
related_issues:
  - "sjawhar/legion#1021"
  - "LEGION-42"
symptoms:
  - "a reviewer files a behaviour gap under Fast-follow because it is 'not a defect of this PR' and the merged code carries it"
  - "a tester cannot satisfy an acceptance line as worded and the round stalls on a permission nobody will grant"
  - "the same rejected option is proposed again two rounds later"
---

# Behaviour findings land in the PR; a re-scoped acceptance records what it rejected

## Context

LEGION-42 (#1021) changed which GitHub App each Legion role acts as. Three review rounds
followed a clean first implementation. None reopened the design; each landed one or two items
the reviewer had classified as "not a defect of this PR" and the architect reclassified as
behaviour this PR *causes* — plus the wording nits that lived in the same files. The rounds cost a
day. The lines that decided what went where are worth writing down, because they were applied
correctly and were still expensive.

## Where a finding goes

The `legion-worker` skill says correctness fixes land in the PR and cleanup is one named
Fast-follow line. The hard cases are findings that are *true before the PR* but become
*load-bearing because of it*:

| finding (round) | reviewer's call | architect's call | why |
| --- | --- | --- | --- |
| boot probe leases only the implement App; a review-less `legion.yaml` starts and 500s on the architect's first `legion gh` (r1) | observation for the architect | **in-PR** | the PR made the architect act as the review App; the gap is new behaviour of this change |
| dead `GitHubService.gh()` with an implicit `"implement"` default (r1) | Fast-follow | **in-PR** | it is the last seed of the drift the PR removes |
| `--check-config` prints `Config OK` for a config the daemon now refuses at boot (r2) | observation, "pre-existing in kind" | **in-PR**, spec Errors row updated | the PR turned a tolerated shape into a refused one; the loader must say so |
| orphaned `runner` plumbing after the `gh()` deletion (r2) | Fast-follow | **in-PR** | consequence of an in-PR change |
| `docs/solutions/*` stating the old mapping / the REST refusal text for a git push (r1, r2) | Fast-follow | **retro** | `docs/solutions/` is retro's file; nothing runs from it |
| comment wording, test-shape nits, an orphaned line wrap (r2) | Fast-follow | **in-PR**, because the round already touched those files | zero marginal review cost |

The rule that falls out: **"pre-existing" is not a disposition.** Ask whether the PR makes the
thing matter. If the change makes a previously harmless state (one App configured, an unused
default) into a failure or a hazard, it is this PR's behaviour and lands here, spec row and
test included — whatever the reviewer labelled it. Wording and `docs/solutions/` stay out of the
diff unless the round is already in that file.

Two costs to expect and plan for:

- **Each in-PR reclassification is a corrective round**: implementer push → tester round →
  reviewer round. Three findings surfaced one round at a time cost three rounds. A reviewer that
  lists *every* behaviour-class observation in its first round (not just the one it is sure of)
  lets the architect land them together. The r2 reviewer did this — its `observationsForArchitect`
  named the `--check-config` gap explicitly as "a behaviour change, so land here or file a LEGION
  issue, not Fast-follow" — and that is the form: name the class, not just the fact.
- **A tightened contract touches every fixture that built the old shape.** Requiring both Apps at
  config load meant 36 test overrides, 3 yaml fixtures, and 5 cli fixtures. Do the consolidation
  in the same commit — one `BOTH_APPS` constant, one `resolveWithApps()` helper, one
  `bothAppsYaml` — so the next tightening, and `main`'s next fixture, are one-line edits
  (`../legion/conflict-only-rebases-keep-the-diff-auditable.md`, the MERGEABLE-but-red section,
  is what happens when `main` adds fixtures of the old shape mid-review).

## Re-scoping an acceptance line on evidence

Acceptance 3 said "the tester's commit status and check run are attributed to
`legion-reviewer[bot]`". The implementer's production-like proof showed the check run at 201 and
the commit status at 403: the review App has no `statuses` permission (`GET /app`). The
implementer reported the exact 403 body and both Apps' permission lists to the architect and
kept working; the architect re-scoped the line the same hour to "check run and pull request
comment", with the reasoning in the spec, and recorded under **Rejected**:

> Asking Sami to add the commit-statuses permission to the legion-reviewer App so a tester can
> post a commit status: no Legion role posts one, the check run already carries the verdict under
> the right identity, and the deployment rule is to decide what an engineer can decide. If a
> later design wants tester commit statuses, that design asks for the permission.

What made this cheap and durable:

1. **The evidence went to the architect with the numbers, not a proposal.** Status code, body,
   both permission sets, and the observation that the tester role prompt names check runs and
   comments — enough to decide without a second read.
2. **The implementer did not stop.** The identity fix was correct either way; the proof and PR
   proceeded on the line as re-scoped.
3. **The rejected option is written where the next reader looks.** A spec's *Rejected* section is
   the record that keeps "just grant the permission" from being re-proposed by the next tester who
   meets the same 403; `one-role-keyed-table-decides-which-github-app-acts.md` repeats it beside
   the permission table for the same reason.
4. **The PR body's acceptance list follows the spec version.** Each re-scope bumped the version
   (v2 → v3 → v4 → v5) and the body header said which one it satisfies, so tester and reviewer
- `plan-ruling-that-changes-a-user-decision-goes-back-to-the-user.md` — the case where a
  re-scope is *not* the architect's to make.
## Related

- `../legion/fast-follow-pr-mechanics-and-queue-registration.md` — what happens to the
  Fast-follow line *after* the parent merges; this document is about what may go on it.
- `a-plan-ruling-that-changes-a-user-approved-requirement-goes-back-to-the-user.md` — the case
  where a re-scope is *not* the architect's to make.
- `../legion/one-role-keyed-table-decides-which-github-app-acts.md` — the change these rounds
  shaped.

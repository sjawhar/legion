---
title: "Controller merge gate 2 (required checks): what the gh CLI and the two GitHub queries really answered, live, on this repository and the smoke sandbox"
category: legion
tags:
  - legion
  - controller
  - merge-queue
  - gh
  - required-checks
  - branch-protection
date: 2026-09-13
status: active
module: legion
related_issues:
  - "LEGION-16"
  - "sjawhar/legion#961"
symptoms:
  - "gh pr checks --required --json exits 0 on a pending or failing row, so an exit-code check reads red as green"
  - "the rulesets query answers HTTP 403 on a private free-plan repository, which looks like a permission error"
  - "the classic branch-protection summary answers [] on a repository that uses rulesets, which looks like 'no required checks'"
---

# Controller gate 2: the live reads behind the rule

`skills/legion-controller/SKILL.md` gate 2 is a rule: the buckets of
`legion gh -- pr checks <pr url> --required --json name,state,bucket,link` decide, exit 1 means
only "no rows", and two repository facts — no required checks at all, or a private free-plan
repository that cannot define them — are read from the repository, never inferred from the
absence of rows. This note records where each part of that rule was checked against GitHub, so
the skill can state the rule without carrying cached answers the controller might trust over
its own live read. Everything below is an observation with a date; re-run the commands rather
than reuse the values.

## Environment

- `gh` 2.98.0 (`/home/ubuntu/.mise/installs/gh/latest/gh_2.98.0_linux_amd64/bin/gh` on the
  development box), run as `legion gh -- …` under a Legion grant, so every request carried the
  implement App's installation token, never a personal one.
- Repositories: `sjawhar/legion` (private, rulesets; the repository this skill lives in) and
  `sjawhar/legion-smoke` (private, GitHub free plan; the smoke rig's sandbox).

## Exit codes and buckets (2026-09-12, `sjawhar/legion`)

With `--json`, the command exited 0 whenever rows existed, whatever their buckets: a `pending`
row and a `fail` row both came back with exit 0. Exit 1 appeared only with no rows, with one of
two messages: `no checks reported on the '<branch>' branch` (nothing had reported at the head
yet) or `no required checks reported on the '<branch>' branch` (checks existed, none required).
Without `--json` the CLI exits 8 for pending rows and 1 for a failing row or no rows; the
controller never runs it that way.

The five buckets the CLI emits (`gh pr checks --help`): `pass`, `skipping`, `pending`, `fail`,
`cancel`. A job skipped by its `if:` reports `skipping`, and GitHub treats a skipped job as
satisfying a required check; this repository's `Legion Envoy and Contracts` workflow skips most
of its jobs through `needs.changes` on most pull requests, so `skipping` rows are routine here.

## Where required checks live (2026-09-12, both repositories)

Two places, read with two queries:

```text
legion gh -- api repos/<owner>/<repo>/rules/branches/<base> --jq '[.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context]'
legion gh -- api repos/<owner>/<repo>/branches/<base> --jq '.protection.required_status_checks.contexts'
```

- `sjawhar/legion`: rulesets `["lint","typecheck","test"]`; classic summary `[]`. It uses
  rulesets, so the "genuinely requires no checks" exception never applies there.
- `sjawhar/legion-smoke`: rulesets query HTTP 403 with body
  `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature.", …}`
  (printed by `gh` as `gh: Upgrade to GitHub Pro or make this repository public to enable this
  feature. (HTTP 403)`); classic summary `[]`.
- The admin endpoint `branches/<base>/protection/required_status_checks` answered HTTP 403
  `Resource not accessible by integration` on both repositories under the implement App, which
  is why the gate reads the branch summary instead.
- Neither repository uses classic branch protection for its required checks. A repository that
  does would answer `[]` for rulesets and the check names in the classic summary (GitHub's
  documented shape); that case was not observed.
- The free-plan message names the plan in its head: `Upgrade to GitHub Pro` on a user-owned
  repository (observed on `sjawhar/legion-smoke`); GitHub's documentation gives
  `Upgrade to GitHub Team` for an organization-owned one (not observed). The stable tail
  `make this repository public to enable this feature` is what the gate matches, as a
  substring of the `message` field — the sentence ends with a period inside a JSON wrapper, so
  literal equality never matches.

## Reviewer re-read (2026-09-13, `sjawhar/legion`, pull request #961 round 4)

The classic-summary query on `sjawhar/legion`, read as `{protected, contexts}`, answered
`{protected: true, contexts: []}`. An unprotected branch answers `[]` for `contexts` too, which is
the same answer for this gate. A response with no `protection` object at all yields `null`,
which fails the gate's `[]` match and leaves the gate pending — the safe side.

## Re-verifying

Run the three commands above against the repository in question under `legion gh --`. If a
value here has changed (a new ruleset, a plan change, a CLI whose `--json` exit codes moved),
update the rule in the skill only if the behaviour changed; update this note either way.

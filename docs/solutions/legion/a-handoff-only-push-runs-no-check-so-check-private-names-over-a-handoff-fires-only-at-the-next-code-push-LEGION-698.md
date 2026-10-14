---
title: "A handoff-only push runs no check, so check-private-names over a handoff fires only at the next code push: grep a handoff for private names before committing it"
category: legion
tags:
  - skip-checks
  - handoff
  - check-private-names
  - lint
  - public-repository
date: 2026-10-14
status: active
module: .github/scripts
related_issues:
  - "LEGION-698"
  - "sjawhar/legion#1950"
---

# A handoff-only push runs no check, so check-private-names over a handoff fires only at the next code push: grep a handoff for private names before committing it

Extends docs/solutions/legion/an-implement-handoff-push-runs-ci-in-full-and-cancels-the-code-heads-runs-cite-the-tips-runs-and-give-the-lane-the-code-heads-image-LEGION-631.md.

- `.github/scripts/check-private-names.sh` runs in the required `lint` job over every tracked
  file, `.legion/<issue>/*.json` handoffs included. A push that changes only `.legion/` carries
  `skip-checks: true` and starts no workflow at all, so a private name a planner, tester or
  reviewer writes into a handoff is caught only by the next push that is not handoff-only — the
  implementer's code push — and fails that head's required check for a name the implementer did
  not write.
- Before committing a handoff, run `bash .github/scripts/check-private-names.sh .legion` in the
  workspace (it takes paths; with none it scans the repository). A deployment other than the
  public one is "the other deployment" or `<deployment repo>`, never its project key or name, in a
  handoff as in a commit message or pull request text.
- When the hit is in another role's committed handoff, redact the word in a commit of your own and
  say so in your handoff's deviations; the other role's commit stays as pushed.

## Evidence

On sjawhar/legion#1950 the planner's handoff-only push (skip-checks) carried
`.legion/LEGION-698/plan.json` with the other deployment's project key in a gap-analysis finding;
nothing ran on it. The implementer's first code push would have failed `lint`; a run of
`check-private-names.sh` over the tree before that push reported `::error file=.legion/LEGION-698/plan.json,line=38`,
and the one word was redacted in a commit of the implementer's (`.legion/LEGION-698/implement.json`
deviations, on the branch).

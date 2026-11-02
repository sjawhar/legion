---
title: "Prove a repository setting on a scratch base of the product repository, and drive a protection-gated mutation on your own pull request"
category: testing
tags:
  - live-proof
  - scratch-branch
  - github
  - auto-merge
  - skip-checks
date: 2026-10-30
status: active
module: packages/daemon/internal/ghautomerge
related_issues:
  - "sjawhar/legion#2562"
---

# Prove a repository setting on a scratch base of the product repository, and drive a protection-gated mutation on your own pull request

Two surfaces that look unreachable are reachable, and both are reached without touching the
default branch or spending a runner minute.

## The rules

- **When the variable under test is a repository's own setting, a throwaway sandbox repository is
  the wrong surface**: it has the setting you give it, not the one production has. Use a scratch
  base branch of the product repository instead. A ruleset targets the default branch alone, while
  the repository's merge settings apply to every base, so a scratch base meets the real default
  and nothing reaches the default branch. Quote the live settings beside the result.
- **Send the request text read out of the shipped source**, with a `sed` range over the constant,
  never a copy pasted from a plan or a handoff. That is what binds the proof to the bytes that
  merge.
- **The control is the file's previous revision**, sent on a twin pull request built from branch
  commits of the same shape. A control that differs in more than the revision proves less than it
  appears to.
- **Spend no Actions minutes**: end each scratch head commit with the repository's skip-checks
  trailer, then quote `gh run list --branch <scratch head>` returning no runs. Delete every
  scratch ref and close every scratch pull request in the same run that made them
  (docs/solutions/testing/delete-a-live-proofs-scratch-fixture-first-at-retro-INFRA-390.md).
- **A mutation GitHub refuses unless the base carries protection rules cannot be driven on a
  scratch base at all**, and adding a ruleset needs `administration: write`, which a role App does
  not hold. Drive it on your own pull request against the protected base, in the state production
  reaches it in, arming and disarming in one command — and make a gate that cannot pass the safety
  interlock, not your speed: with a required approving review unmet, nothing can merge while the
  arm stands. The pull request's timeline keeps the record.
- **Split each acceptance criterion into the half a fake can decide and the half only the live
  surface can**, and restate the first as a predicate a machine reads. "The body holds no commit
  message of the pull request" is not something a fake can know; "each arm sends
  `headline == \"<title> (#<n>)\"` and `body == \"\"`" is, and the real claim then belongs to the
  live proof.

## Evidence

The change under proof made the Legion daemon write the squash commit message of every merge it
submits. Its surface was sjawhar/legion itself: a scratch base branch of `main`, a head of two
commits carrying the lines that must not reach a base branch, and a pull request of one onto the
other. The repository's live settings, quoted with the result, were
`squash_merge_commit_message=COMMIT_MESSAGES` and `squash_merge_commit_title=COMMIT_OR_PR_TITLE` —
the very default the change defeats, which a sandbox repository would not have had.

The mutation text came from the shipped file by
`sed -n '/^\tmerge = `/,/^}`$/p' packages/daemon/internal/ghautomerge/ghautomerge.go | sed '1s/^[^`]*`//; $s/`$//'`,
and the control from the same file at the revision before the change
(`jj file show -r <base commit> <path>`). Each scratch head ended with the skip-checks trailer and
`gh run list --branch` returned no runs for any of them; every scratch branch was deleted and
every scratch pull request merged or closed.

`enablePullRequestAutoMerge` was refused on every state a scratch base reaches — `Pull request is
a draft`, `Pull request is in clean status`, `Pull request is in unstable status`, and, on a
conflicting pull request whose base had no ruleset, `Protected branch rules not configured for
this branch` — and `POST /repos/sjawhar/legion/rulesets` answered `403 Resource not accessible by
integration`. It was then driven on the change's own pull request, sjawhar/legion#2562, whose base
is the protected default branch and whose state was `BLOCKED`, the state the production arm is
taken in: the mutation was sent, `autoMergeRequest { commitHeadline commitBody mergeMethod }` read
back, and auto-merge disabled, in one command, with none of the one required approving review in
place throughout.

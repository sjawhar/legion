---
title: "gh run list --commit takes the full forty-character sha, checks belong to a commit and not to a pull request, and an empty answer is not a measurement"
category: legion
tags:
  - github
  - ci
  - gh-cli
  - evidence
date: 2026-10-25
status: active
module: skills
related_issues:
  - "INFRA-92"
  - "sjawhar/legion#2154"
---

# gh run list --commit takes the full forty-character sha, checks belong to a commit and not to a pull request, and an empty answer is not a measurement

Three readings of the forge that look like findings and are not. Each cost this issue time, and
two of them nearly cost a force-push on a clean branch.

- **`gh run list --commit` matches the full forty-character sha only.** An abbreviated id, and a
  forty-character string you built by padding one, both return an empty list and exit 0. Resolve
  the id first — `jj log -r <short> --no-graph -T commit_id` — and never extend a short commit id
  into a full sha by hand.
- **A check run belongs to a commit, not to a pull request.** `gh pr checks` reports the checks of
  the pull request's **head**. Before sjawhar/legion#2354 a branch head could be a handoff-only
  commit, which carried `skip-checks: true` by design and started no workflow. `gh pr checks` then
  showed only the checks the pull-request event itself produced, while the code beneath it was
  checked at its own commit, and the daemon carried the code head's verdict to the handoff head.
  A handoff is a record on the Dispatch issue now, so every pushed head is a code head and runs
  CI. Read `gh api repos/{owner}/{repo}/commits/<commit-sha>/check-runs` at the commit you judge
  before concluding a head is unchecked.
- **An empty answer is not a measurement until the query is known to be well formed.** A query
  that returns nothing for a malformed argument and nothing for a true absence cannot tell you
  which you have. Give it a case whose answer you already know — a commit you are certain has
  runs — and only then believe the empty one.
- **A claim about another tree's branch is read at that branch's head, resolved at the moment you
  make the claim.** `gh api repos/<owner>/<repo>/contents/<path>?ref=<head>`, never
  `gh pr diff --patch`, which replays every intermediate commit and so still shows text a later
  commit deleted. A head resolved an hour ago is not the head; say in the claim which head you
  read and when.
- **Every proof carries a control.** A case whose answer is already known, run beside the one you
  are measuring, is what separates a fix from a coincidence.

## Evidence

INFRA-92 twice read `gh run list --commit 301288e631cb` and `--commit <a forty-character string
padded from it>`, got an empty list from both, and concluded the code commit had no runs. The
real id was `301288e631cbbbd02f3fb22673bf42d00c658d55`; with it the same query returns `Tests`,
`Docs`, `PR Title` and `Legion Envoy and Contracts`. The tester's handoff carried the same
mistake and was corrected in its own round (`test: correct the handoff — the code commit's CI was
read with an abbreviated sha`).

Separately, the architect directed a force-push to drop a `skip-checks: true` trailer from the
branch head, on the reading that three required checks had never run on that content. Reading
the code commit's `check-runs` showed `changes`, `build` and `claude-envoy` green with `dispatch`
still running: the content was checked, at the commit below the head, which is the designed
shape. The direction was withdrawn and nothing was rewritten.

The controls this issue's accepted proofs used: `main@origin`'s bundle beside the new one for the
registered title; an identically aged abandoned role record beside the live holder's for the
sweep; the previous head answering a 200 with HTML beside the new one for the restore retry.

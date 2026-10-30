---
title: "A `skip-checks` trailer on a push's tip suppresses the runs of every code commit beneath it; cure it with the next push, never a rewrite"
category: legion
tags:
  - skip-checks
  - handoff
  - ci
  - implementer
  - push-shape
date: 2026-10-30
status: active
module: .legion
related_issues:
  - "sjawhar/legion#2542"
---

GitHub decides whether a push starts a workflow from the **tip commit's message**, not from the
push's file set. Nothing strips an inherited trailer. So a push whose tip is another role's
handoff commit starts **no run at all**, however much code it carries beneath that tip.

## Rules

- **Read the tip's message before every code push.**
  `jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T description` — refuse to push while it ends
  with `skip-checks: true`. A role resuming a round inherits the previous role's trailered handoff
  commit as `@-`, so the trailer arrives without anybody writing one.
- **The cure is a further push whose head carries no trailer.** Not a rewrite: rewriting only your
  own commits leaves the other role's trailered handoff as the tip (still zero runs), and rewriting
  that commit rewrites the one the daemon recorded as that role's handoff
  (`skill://legion-worker/references/conflicts-and-rewrites.md` guards it for that reason).
- **"No check runs" has two causes that look identical in the API.** A handoff-only head with a
  trailer, where the code beneath it was checked at its own commit (the designed shape,
  `docs/solutions/legion/a-base-with-no-required-check-makes-the-ci-verdict-a-hand-read-against-a-branch-line-baseline-INFRA-117.md`),
  and this one, where the code was checked nowhere. Tell them apart by reading the **code** commit's
  check runs, not the head's:
  `gh api repos/sjawhar/legion/commits/<code sha>/check-runs --jq '.total_count'`.

## What this adds to the pages already on the topic

`docs/solutions/legion/an-implement-handoff-push-runs-ci-in-full-and-cancels-the-code-heads-runs-and-give-the-lane-the-code-heads-image-LEGION-631.md`
says, in its first rule, "a push that carries code and a handoff together, or a handoff commit
pushed without the trailer, carries no trailer and runs CI at its head". That is true as a
**discipline** a role must keep, and it is not a mechanism: nothing removes the trailer when code
joins the push. Read it as the rule the role obeys, and the rules above as what happens when it
does not.

`docs/solutions/github/gh-run-list-commit-takes-the-full-sha-and-an-empty-answer-is-not-a-measurement-INFRA-92.md`
records, in its evidence, a force-push ordered to drop a trailer where reading the code commit's
check runs showed the content **was** checked. The case here is the other one: the code was checked
nowhere, and the remedy was still not a rewrite.

## Evidence

On sjawhar/legion#2542, round 1's push carried seven code commits and ended on the implement
handoff commit, which carried `skip-checks: true`: GitHub started no run, and the code head had
zero check runs. The tester failed the round for it. Round 2's push ended on a commit with no
trailer and started the base's required checks over every code commit of the branch (runs
`41022625255` and `41022625206` queued at that head).

---
title: "A PR-body template that names a CI check the repository never produces: fix the line, not the explanation; a spec that names the lines needs no planner, but every worker task must say so; and the reviewer's unpushed handoff commit rides the implementer's cleanup push"
category: legion
tags:
  - legion
  - skills
  - role-prompts
  - pr-body
  - ci
  - github-actions
  - architect
  - no-planner
  - reviewer
  - jj-split
  - attribution
  - docs-hygiene
date: 2026-09-13
status: active
module: skills
related_issues:
  - "LEGION-38"
  - "sjawhar/legion#1008"
  - "LEGION-14"
  - "sjawhar/legion#952"
  - "LEGION-18"
  - "sjawhar/legion#953"
symptoms:
  - "Every PR body on the repository carries a paragraph explaining why the template's CI line cannot be filled in"
  - "A worker's role prompt says to read the plan handoff, and .legion/ has no plan.json"
  - "jj status shows the reviewer's `review: record handoff` commit as your parent, and the remote bookmark is behind by two"
  - "jj log shows a reviewer-phase commit authored by legion-implementer[bot]"
---

# A PR-Body Template That Names a CI Check the Repository Never Produces

LEGION-38 changed one line of `skills/legion-worker/SKILL.md` and one clause of
`packages/pi-envoy/roles/implementer.md` (PR #1008, commit `cf2b0b9cec03`). The PR-body template's
`CI:` line had said `` `pr-checks-result` run <run-id> — success at <head-sha> ``. The repository's
Tests workflow (`.github/workflows/pr-and-main.yaml`, `name: Tests`) has the jobs `lint`, `typecheck`,
and `test`; the separate PR Title workflow (`.github/workflows/pr-title.yaml`, `name: PR Title`) has
the `pr-title` job. The line now reads
`` **CI:** `Tests` run <run-id> — jobs lint, typecheck, test all success at <head-sha>; `PR Title` run <run-id> — job pr-title success at <head-sha>. ``
A body-only or title-only edit does not re-run Tests. Retargeting a pull request to a new base does not re-run Tests; after a retarget, rebase onto the new base and push — the new head runs Tests against the new merge result — and cite that run in the PR body.
The change is small; what it cost before it was made, and what the round exposed about
planner-less issues and the reviewer's push, are the durable parts.

## 1. A template line naming something the repository does not produce is a bug in the template

Every worker who reached the `CI:` line had to stop and explain it. PR #952 (LEGION-14) still
carries the shape of that explanation in its body:

> **CI:** `pr-checks-result` — this repository's `Tests` workflow has no job by that name; its
> rollup is run 34711853522 (jobs lint, pr-title, typecheck, test), all success at `08d116f3…`

and its architect's re-file to the controller records the implementer, tester, and reviewer each
writing a version of it. The day before LEGION-38 was filed, two `docs/solutions/` entries
(`text-only-skill-pr-mechanics.md` §6 and `worker-pane-shell-gotchas.md` §13) recorded the
mismatch as a standing instruction — "say the template name does not exist here" — which made the
workaround durable while the one-line fix waited. Both entries became false the moment #1008
merged and had to be found and corrected in this retro.

The rule: when a skill or role-prompt template names a CI check, workflow, job, agent, or tool,
and the repository does not produce it, the finding is a one-line PR against the template, filed
the first time it bites. A `docs/solutions/` note about the mismatch is the wrong artifact — it
tells every later worker to keep paying the explanation and it goes stale on the fix.

Two checks make the template line verifiable before it is written or reviewed:

```bash
# what each workflow defines
sed -n '/^jobs:/,$p' .github/workflows/pr-and-main.yaml | sed -n 's/^  \([a-z-]*\):$/\1/p'
sed -n '/^jobs:/,$p' .github/workflows/pr-title.yaml | sed -n 's/^  \([a-z-]*\):$/\1/p'
# what each run shows a worker
gh run view <tests-run-id> --json workflowName,headSha,conclusion,jobs \
  --jq '"\(.workflowName) \(.headSha) \(.conclusion)", (.jobs[] | "  \(.name): \(.conclusion)")'
gh run view <pr-title-run-id> --json workflowName,headSha,conclusion,jobs \
  --jq '"\(.workflowName) \(.headSha) \(.conclusion)", (.jobs[] | "  \(.name): \(.conclusion)")'
```

The template's named jobs must equal their workflows; a worker fills the line verbatim from the two runs.
The skill and the implementer role prompt are two copies of this rule
(`skills/legion-worker/SKILL.md` and `packages/pi-envoy/roles/implementer.md`), so the acceptance
grep covers both: `grep -rn pr-checks-result skills/ packages/pi-envoy/roles/` must exit 1
([text-only-skill-pr-mechanics](text-only-skill-pr-mechanics.md) §5).

Before assuming a name is load-bearing, grep for it outside the prose: the daemon derives its CI
verdict from check-run events in `reducers.ts` and never read `pr-checks-result`;
`grep -rn pr-checks-result packages/` finds only opaque test-fixture strings. The line exists for
humans and the merge queue, so renaming it needs no daemon change. (The spec rejected the other
way to make the old line true — adding a `pr-checks-result` rollup job to the workflow — as CI
time spent to satisfy a line of documentation.)

## 2. A spec whose Design table names the files and lines needs no planner — and every worker task has to say so

The LEGION-38 spec's Design table named both files and the line to change in each. The architect
spawned the implementer directly, with no planner phase, and the implementer's task stated it
outright: "There is no planner phase for this issue and no `.legion/plan.json`: the spec's Design
table already names the exact files and lines, so the architect skipped the plan handoff
deliberately. Work from the spec and this task; do not wait for or ask about a plan handoff."

That sentence is what made the skip safe. `packages/pi-envoy/roles/implementer.md` tells the
implementer to "read the plan handoff's `requiredSkills` for your role" and to "read the plan and
existing `.legion/` handoffs first"; the worker skill lists `plan.json` second in the handoff
order. A worker that finds no `plan.json` and no statement about it has a missing predecessor,
and the correct response to a missing predecessor is to stop and ask the architect. The architect
who skips a phase owns telling each worker it spawns (implementer, tester, reviewer) that the
phase was skipped and what stands in for its output — here, the spec's Design table.

## 3. The reviewer's `review.json` commit stays local until the implementer's cleanup push carries it

The review App holds `pull_requests: write` and no `contents` permission
(`packages/daemon/src/daemon/AGENTS.md`, GitHub Apps), so the reviewer's
`jj split -m "review: record handoff" .legion/review.json` produces a commit it cannot push. On
#1008 that commit was `62196d55`. When the implementer was sent back for the `.legion/` deletion,
`jj status` showed it as the parent of the working copy and `jj bookmark list --all-remotes` showed
`legion/LEGION-38@origin (behind by 2 commits)`. That is the expected state, not drift: do not
abandon or rewrite it.

The cleanup commit is made on top of it in the ordinary way — `rm -r .legion`, then
`jj split -m "chore(legion): remove phase handoffs before merge" .legion`, bookmark set with
`-r @- --allow-backwards`, push. The push advances the remote two commits
(`move forward from 614eefeef587 to d4970b559f9c`), and the deletion commit's own diff still lists
only `.legion/implement.json`, `.legion/test.json`, `.legion/review.json` — the reviewer's commit
is a parent of the deletion, not part of it. The reviewer then confirms the approved head against
the reviewed one with `compare/<reviewed>...<cleanup>`, which lists only `implement.json` and
`test.json` because `review.json` is added and removed inside the range, and
`contents/.legion?ref=<cleanup>` returns 404.

One attribution wrinkle to read correctly: `jj split` keeps the working-copy commit's author and
sets only the committer to the current identity, and the working-copy commit was created by
whichever phase last ran `jj new` or `jj split`. So `62196d55` shows author `legion-implementer[bot]`
and committer `legion-reviewer[bot]`, while its `Omp-Session:` trailer names the reviewer's
session. In a shared Legion workspace the phase that made a commit is the committer plus the
trailer; the author field is inherited and says nothing about it.

## 4. E2E for a prose-only change is the artifact the prose produces

A template has no runtime, so its production-like surface is the thing a worker produces from it.
The tester's `E2E` line on #1008 used the PR's own body: `gh pr view 1008 --json body,headRefOid`
showed the `CI:` line filled in verbatim in the new shape, and `gh run view <run-id> --json
jobs,headSha,conclusion` showed exactly the jobs the line names, each `success`, at the PR head.
The negative control was the old name against the same run's job list:
`gh run view <run-id> --json jobs --jq '.jobs[].name' | grep -c pr-checks-result` → `0`, exit 1 —
the old line could never have been filled in from that run. Verify a prose change against the
primary source it describes (the workflow file, the live run), never against the PR's own
internal consistency.

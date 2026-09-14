---
title: "A skill instruction that names a public action is an unguarded write: the tool boundary refuses it, the text only asks"
category: legion
tags:
  - legion
  - skills
  - legion-gh
  - worker-bin
  - public-action
  - refusal
  - dispatch
  - github-issues
  - acceptance-grep
date: 2026-09-13
status: active
module: packages/daemon/src/cli, skills
problem_type: safety
severity: high
related_issues:
  - "LEGION-78"
  - "sjawhar/legion#1015"
  - "LEGION-27"
  - "LEGION-45"
applies_when:
  - A skill, role prompt, or template tells a worker to run a command whose effect is public or irreversible (a GitHub write, a push, an op-log rewind, a message outside the company)
  - A worker followed such an instruction in good faith and the instruction was wrong
  - You are deciding whether a text fix is enough or the command itself must refuse
symptoms:
  - "A Legion bot account posted a comment on a GitHub issue nobody in the tree had touched"
  - "The retro skill's template said `legion gh -- issue comment <issue-number>` and the issue number was a Dispatch key's digits"
  - "The same rule had already been written into a skill once and was still broken by a worker who never saw that line"
---

# A skill instruction that names a public action is an unguarded write: the tool boundary refuses it, the text only asks

## What happened

On 2026-09-13 the LEGION-27 retro followed the template then in `skills/legion-retro/SKILL.md`:

```bash
legion gh -- issue comment <issue-number> --body $'## Retro Complete …' --repo <owner>/<repo>
```

Legion issues live on Dispatch and never map to a GitHub issue number, so `<issue-number>` became
`27` and the comment landed on the unrelated public issue sjawhar/legion#27. The worker noticed
and deleted it within a minute, then posted correctly with `dispatch_message` on LEGION-27. Nothing
in the worker's environment could have stopped it: `legion gh` forwarded any `issue …` argv, and
the root `AGENTS.md` sentence "GitHub issues are never read or written by Legion" is a rule a
worker reads, not one the command enforces.

This was the second time skill text alone had failed as the guard for a public or irreversible
action. The first was `jj undo`/`jj op restore` in a shared issue workspace (LEGION-45): the
prohibition was in the worker skill, another workspace ran it anyway, and it rewound this
workspace's operations
([shared-main-repo-hazards](shared-main-repo-hazards-for-concurrent-issue-workspaces.md) Hazard 2,
[long-lived-branch-mechanics](long-lived-branch-mechanics-jj-new-and-merge-not-rebase.md)). No
code guard exists for that one yet.

## The rule

When an instruction names a command whose effect leaves the workspace — posts, pushes, merges,
rewinds shared state, messages anyone — the durable fix is not a better sentence. It is a refusal
at the boundary every worker's command already passes through, with the sentence corrected in the
same change so the two agree. A worker who never reads the corrected sentence (a stale plugin
release, a copied template, a role prompt from another branch) still hits the refusal.

Legion has exactly one such boundary for GitHub: `legion gh` in `packages/daemon/src/cli/index.ts`
(`cmdGh`), which every pane's `gh` shim (`<state_dir>/worker-bin/gh`) execs. It already refused
`pr merge` and raw `gh api …/merge` (`isGhMergeIntent`). LEGION-78 added the sibling
`isGitHubIssueWriteInvocation` beside it — every `issue` write verb, and any raw `gh api` call to
an `/issues` path whose effective method is not GET
([how that classification works](../github-api/classify-a-raw-gh-api-call-as-a-write-by-its-effective-method.md)) —
and one `if` in `cmdGh`:

```ts
if (isGitHubIssueWriteInvocation(args)) {
  throw new CliError(
    `Legion issues live on Dispatch; use dispatch_message or dispatch_comment on ${deps.env.LEGION_ISSUE || "the Dispatch issue"}`
  );
}
```

Three properties make this shape worth copying for the next boundary:

1. **The refusal precedes credential redemption.** Both `if`s sit before `redeemGitHubToken`, so a
   refused command reads no grant and makes no daemon call. The tests assert it (`fetchCalled`
   and `spawnCalled` both `false`), and the tester proved it live: with
   `LEGION_GRANT_FILE=/nonexistent/grant` the refused `issue comment` still prints the Dispatch
   message and exits 1, while a forwarded `pr view` under the same environment fails on the
   unreadable grant file.
2. **The message names the right destination, not just the wrong one.** `LEGION_ISSUE` is on
   every pane, so the refusal tells the worker which Dispatch issue to post on and which tools to
   use. A refusal that only says "no" sends the worker looking for a workaround.
3. **The over-refusals are chosen and written down.** A flag value equal to a verb
   (`issue list --search close`), a non-GET call whose flag value contains `/issues/`, and a raw
   edit of a pull-request *conversation* comment (`…/issues/comments/<id>` — GitHub serves those
   from the issues endpoint; `gh pr comment --edit-last` / `--delete-last` remain available) are
   all refused, and the function's doc comment says so. The `pr merge` guard has the same
   imprecision for `/merge`. Accepting a few refused reads is the price of never forwarding a
   write; the tests pin the forwards that matter (`pr comment`, `pr review`, `api …/pulls/…`,
   `api graphql`, `issue view`/`list`, any GET on an `/issues` path).

## The text side still gets fixed — and locked by a grep

The template became the `dispatch_message` call (footer kept, `phase: "retro"`), every "issue
comment" phrase in the retro and architect skills became "Dispatch message", and the two places
that document what the shim refuses (root `AGENTS.md` line 48, the worker skill's `gh` paragraph)
were extended in the same PR. The spec's acceptance check for the text is three greps that must
print nothing:

```bash
grep -rn 'gh -- issue\|gh issue\|issue comment <' skills/ packages/pi-envoy/roles/
grep -rn 'issue comment' skills/legion-*/ packages/pi-envoy/roles/
grep -rn 'gh issue' skills/ packages/pi-envoy/roles/
```

That constrained the new prose: the refusal sentence names the verbs as "the `issue` subcommand's
`comment`, `create`, …" and never spells `gh issue`, so the greps stay a regression lock rather
than a one-time check. When you add a forbidden phrase to a skill's acceptance, write the
replacement prose so it cannot match — otherwise the lock is deleted by the first honest edit.

## Why the text alone was never going to hold here

The tester demonstrated the hazard live during LEGION-78's own test phase. The branch carried the
fixed skill and the refusing shim; the pane's `legion` still ran the deployed daemon checkout, and
the pane's skills still came from the installed plugin release — both pre-change. Running the
refused command once through the pane's `legion` posted a `smoke` comment on the PR
(id 5652558869) that had to be deleted. Every worker in every tree on the deployment was in that
position until the checkout carried the merge, whatever the branch said. The details of those two
deployment surfaces are in
[worker-pane-shell-gotchas](worker-pane-shell-gotchas.md) §13.

## Checklist for the next one

- Is the action public or irreversible? Then find the boundary it must pass (a shim, a CLI
  subcommand, a credential helper, an API route) and refuse there, before any credential is used.
- Correct the instruction in the same change; make the acceptance a grep the new prose cannot
  match.
- Put the right destination in the refusal message.
- Enumerate the over-refusals in the predicate's doc comment and pin the forwards in tests.
- Prove the refusal through the branch's own entry point, not the pane's deployed one, and say so
  in the PR body's `E2E` line.

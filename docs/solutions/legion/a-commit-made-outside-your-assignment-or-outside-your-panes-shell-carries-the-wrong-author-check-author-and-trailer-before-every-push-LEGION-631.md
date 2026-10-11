---
title: "A commit made outside your assignment, or outside your pane's shell, carries the wrong author: check author and trailer before every push"
category: legion
tags:
  - jj
  - commit-identity
  - working-copy
  - metaedit
  - Omp-Session
  - divergent
date: 2026-10-10
status: active
module: packages/pi-legion
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# A commit made outside your assignment, or outside your pane's shell, carries the wrong author: check author and trailer before every push

Extends docs/solutions/legion/jj-commit-identity-is-per-process-and-the-working-copy-is-adopted-at-assignment.md.

- That note's adoption runs at assignment, for the role the daemon just started. A push you make on
  the architect's word while another role's phase runs splits your work out of a working copy the
  daemon adopted for that role, and Fact 2 keeps its author: the commit lands as the other App's.
  Before every push, read `jj log -r 'legion/<KEY>@origin..@-' -T 'author.email()'` and the
  `Omp-Session:` trailer; re-author with the pane's own identity,
  `jj metaedit --update-author -r <rev>` under your `JJ_USER`/`JJ_EMAIL`, before the push.
- A commit made from anywhere but the pane's shell — a kernel, a subprocess, a subagent's runner —
  has neither the identity nor the attribution overlay: `JJ_USER`/`JJ_EMAIL` are the pane's
  environment, and the `Omp-Session:` trailer is a `templates.commit_trailers` overlay the extension
  hands the pane's shell. Run jj with the pane process's environment (`/proc/<omp pid>/environ`) and
  `JJ_CONFIG=<user config>:<overlay>`; the trailer is appended on a description change, so a commit
  whose message is already final needs a re-describe (to a temporary message and back) to gain it.
- After `metaedit` rewrites a commit, address it as `@-` or by change id, never by the commit id you
  read before the rewrite: a `jj describe -r <old commit id>` creates a divergent sibling of the
  change. Fold it with `jj squash --from <stale> --into <kept> --use-destination-message`, then
  check `jj log -r 'change_id(<id>)'` prints one commit.
- The workspace has one working copy, and the daemon adopts it for the next role at that role's
  assignment. When a phase changes while your merge is still unpushed, `@` becomes an empty,
  undescribed commit of the other App's above your merge, and `legion push` refuses that commit at
  `@-`. Put `@` directly on the commit to push (`jj new <merge commit>`), push, and tell the next
  role to `jj new` onto the pushed head before it commits, so its handoff commit is its own App's
  (`handoff_complete` refuses one another App authored).

## Evidence

sjawhar/legion#1843, round 8: 685fc8c6 (the merge-arming prompts) is authored
`legion-reviewer[bot]` because it was split from the empty working copy the daemon had re-authored
for the reviewer's phase; its `Omp-Session:` trailer is the implementer's. Review round 3: the bash
tool dropped mid-commit; the commit made through the kernel carried the reviewer's author and no
trailer, `jj metaedit --update-author` under the pane's environment and a re-describe fixed both, and
a describe by the pre-rewrite commit id left `mtyoxuvu` divergent until `jj squash --into` folded
it (eabefdc2 is the single result). The forward merge dcadda9b was pushed after the reviewer had moved
the issue to testing: `legion push` first refused the empty, reviewer-App-authored `@-` the adoption
had made above the merge; `jj new dcadda9b` and a second push landed it, and the tester was told to
`jj new` onto it.

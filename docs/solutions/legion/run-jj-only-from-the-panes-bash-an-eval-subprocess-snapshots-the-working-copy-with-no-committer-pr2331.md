---
title: "Run jj only from the pane's bash: an eval subprocess snapshots the working copy with no committer"
category: legion
tags:
  - jj
  - identity
  - eval
  - push
date: 2026-10-28
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2331"
---

# Run jj only from the pane's bash: an eval subprocess snapshots the working copy with no committer

- **Run every jj command from the bash tool.** That includes read-only ones such as `jj resolve
  --list` and `jj diff`. Never run jj from an eval kernel's `subprocess` or `!cmd`. The pane's
  identity is `JJ_USER` and `JJ_EMAIL` in the bash tool's environment, and the eval kernel does not
  have them.
- **Why a read-only command still matters:** any jj command snapshots the working copy first. A
  snapshot taken without an identity rewrites the working-copy commit with an empty committer.
- **What it costs:** `jj git push` refuses that commit with `has no author and/or committer set`.
- **Fix it on an unpushed commit of your own** with
  `jj -R "$LEGION_WORKSPACE" metaedit -r <commit> --force-rewrite` from bash. This sets the committer
  and keeps the tree. `--update-author` alone does nothing when the author is already right.
- **Then fix every reference to the old SHA.** The commit gets a new SHA, so update each place that
  named the old one, such as a handoff's `headSha` or a fingerprint comment, before you push.
- Edit files from eval if you like, but leave jj to bash.

## Evidence

- On merge 1c509398, a `subprocess.run("jj -R . resolve --list …")` from the Python eval kernel ran
  after a conflict was resolved in eval. The push was refused (`Won't push commit 1c509398d939 since
  it has no author and/or committer set`). `metaedit --force-rewrite` produced dfd2ff0a with the
  same tree.
- The next merge, 0d4104f9, hit the same thing from a `jj restore` run in eval (jj printed the
  `jj config set --user user.email` hint). It was fixed the same way before the push.

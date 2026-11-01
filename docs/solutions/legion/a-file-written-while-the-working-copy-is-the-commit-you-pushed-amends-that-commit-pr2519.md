---
title: "A file written while the working copy is the commit you pushed amends that commit"
category: legion
tags:
  - jj
  - working-copy
  - handoff
  - forward-merge
  - recovery
date: 2026-11-01
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2519"
---

# A file written while the working copy is the commit you pushed amends that commit

jj has no staging step: the working copy *is* a commit, and the next jj command snapshots whatever
is on disk into it. So a push does not seal the commit it pushed. Write one more file while that
commit is still `@` — a handoff, a note, a scratch file you meant to delete — and the next jj
command folds it in, giving the commit a new id while the remote keeps the old one.

- **After a push whose commit you have reported anywhere, run `jj new` before you write anything
  else.** A commit id in a pull request comment, a handoff, a message to another role or a CI run
  is a claim about bytes; it stops being true the moment the commit is amended.
- **Recover with the operation log, not by editing the tree.** `jj op log` names the push;
  `jj op restore <that operation>` puts the commit back exactly as pushed, with its id. Restoring
  a path, or deleting the file and committing again, gives a commit with the same content and a
  different id, which is the thing you are trying to avoid.
- **Copy the file out first.** `jj op restore` takes the working copy with it, so save what you
  wrote to `$TMPDIR` before restoring, then `jj new` and write it into the child.
- The same trap applies to any tool that writes into the workspace while `@` is a commit you care
  about — a formatter, a generator, a subagent.

## Evidence

Two rounds of sjawhar/legion#2519, both forward merges. Each pushed a merge commit, reported its
sha in a pull request comment carrying the merge fingerprint, and then wrote
`.legion/INFRA-625/implement.json` while `@` was still that merge commit. The next `jj` command
snapshotted the handoff into the merge: `2898fcf0` became `1569be81` and `9125d44d` became a
commit of its own, in each case with the comment on the pull request naming a sha the branch no
longer held.

Both were recovered the same way: copy the handoff to `$TMPDIR`, `jj op log --limit 6` to find the
`push bookmark legion/<issue> to git remote origin` entry, `jj op restore <that id>` — which
reported `Added 0 files, modified 0 files, removed 1 files` and left `jj log -r @` printing the
pushed sha again — then `jj new`, copy the handoff back, describe and push it as a child. The
merge commit the fingerprint comment names is then byte-for-byte the one on the remote.

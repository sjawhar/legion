---
title: "Stop the pipeline that writes to main, rather than re-merging the branch it conflicts with"
category: github
tags:
  - release
  - conflicts
  - forward-merge
date: 2026-10-31
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2607"
---
# Stop the pipeline that writes to main, rather than re-merging the branch it conflicts with

- When a pull request goes `CONFLICTING` on the same path again and again, find what *writes* that path on the default branch and stop the writer.
  Another forward merge buys one round; the writer sets the rate.
- A pipeline that commits generated edits back to its own default branch is such a writer: version bumps, lockfiles, regenerated bundles, committed build output.
  Its publish rate is the conflict rate of every long-lived branch that touches those files.
- Measure both before you choose: how often the writer lands on that path, and how long the branch needs between a forward merge and its merge.
  When the first is shorter than the second, no amount of re-merging ends it.
- Stopping the writer is a change of its own, with its own issue, review and merge.
  It is cheaper than the rounds it prevents when the branch is a deletion, because a deleted file conflicts with every edit of it, not only with an overlapping one.

## Evidence

- sjawhar/legion#2599 deletes `packages/envoy-plugin`.
  `release.yaml`'s `publish` job rewrote `packages/envoy-plugin/package.json` in place (`jq '.version = $v'`, in `envoy_plugin_build` and again in `publish`), committed it (`release-push.sh tag packages/envoy-plugin`) and pushed it to `main` (`release-push.sh push-main packages/envoy-plugin`) on every release.
  Each of those pushes is a `modify/delete` conflict against #2599.
- The rate: `main` released that plugin five times on 2026-10-31, v12.5.0 to v12.7.0, two of them between a forward merge of #2599 and the moment it could have merged (13:51Z and 15:21Z).
  #2599's head moved `677d6ee5` to `35ea1346` during the single day INFRA-909 ran, and it was still `OPEN` and `CONFLICTING` at the end of it.
- sjawhar/legion#2607 is the stop: the `envoy_plugin_build` job, its publish steps and its trigger path, 187 deleted lines, four files.
  After it, `grep -nE 'release-push\.sh (push-main|tag) ' .github/workflows/release.yaml` names four write calls, all to the two Oh My Pi plugins, and `packages/envoy-plugin` appears once in the whole file, at the kept trigger entry.
  On `main` the same two commands gave six write calls and twelve lines.
- Related, both of them the coping side of the same problem: `docs/solutions/legion/when-main-conflicts-on-the-file-faster-than-the-required-checks-finish-report-both-rates-to-the-architect-pr2422.md` and `docs/solutions/github/a-branch-that-deletes-files-main-still-edits-ports-each-edit-at-every-forward-merge-and-reruns-its-coverage-checks-on-the-merged-tree-pr2316.md`.

---
title: "A gate that forbids a pattern tree-wide is a gate on the merged tree, and the base will break it for you"
category: github
tags:
  - github-actions
  - forward-merge
  - ci-gates
  - merged-tree
date: 2026-10-30
status: active
module: .github/scripts
related_issues:
  - "INFRA-567"
  - "sjawhar/legion#2512"
---

# A gate that forbids a pattern tree-wide is a gate on the merged tree, and the base will break it for you

Extends docs/solutions/github/a-branch-that-deletes-files-main-still-edits-ports-each-edit-at-every-forward-merge-and-reruns-its-coverage-checks-on-the-merged-tree-pr2316.md.

That page covers a *coverage* check, where the base's new file is an omission your branch must
add. A check that **refuses** a pattern anywhere in the tree is the other shape, and it fails
differently:

- Expect the base to commit a violation while you work. Neither head is red alone: your branch
  has no such reference, the base has no such gate, and only the tree GitHub builds from the two
  holds both. Read a red check at a head you believe clean as a fact about the merge before you
  look for it on your branch.
- Fix it on your branch. The base's code is not yours to leave: the forward merge is the only
  place where the gate and the violation meet, so the merge commit (or a commit on top of it)
  repoints the base's new reference. Say so in the merge's message, since the diff otherwise
  looks like an unexplained edit of someone else's job.
- Count a harness case that asserts the real tree passes as a second red. It fails in the test
  job while the gate fails in the lint job, with messages that do not name each other, so a
  reader who fixes one is surprised by the other.
- Re-run the gate over the whole merged tree after every forward merge, not only over your own
  diff, and say in the handoff how many files it swept: the number moves with the base.

## Evidence

sjawhar/legion#2512 added `.github/scripts/check-ci-images.sh`, which refuses any CI-reachable
reference to a third-party registry. `main` then added the `envoy-go-race-docs` job at `de546ed2`
(#2516) with a `services:` Postgres on `mirror.gcr.io/pgvector/pgvector`. Run 41009980170 at the
branch's own head `e4d33f45` was red twice for one cause: `lint` (job 123919645492) printed
`.github/workflows/ci.yaml:2476: the postgres service's image names mirror.gcr.io/pgvector/pgvector`
and `2 problem(s)`, and `test` (job 123919645418) printed `FAIL: the real tree passes the tree
mode`, `Results: 97 passed, 2 failed`, from the harness case in
`.github/scripts/check-ci-images.test.sh`.

The fix was entirely on the branch: forward merge `a2056893`
(`jj new legion/INFRA-567 main@origin`, never a rebase), with that service repointed at
`ghcr.io/sjawhar/legion/ci@sha256:2358fcba…`, the digest `.github/ci-images.yaml` already held.
The unchanged-diff fingerprint moved by exactly those three lines, which the pull request's
forward-merge comment quotes.

The sweep count moved with each merge, which is why the handoff records it: 6275 text files at
the branch's own head, 6423 after the first forward merge, 6490 after the second.

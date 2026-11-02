---
title: "GitHub's GraphQL merge message inputs: an empty body is a value, an empty headline is an omission, and an arm freezes both"
category: github-api
tags:
  - graphql
  - squash-merge
  - auto-merge
  - commit-message
  - release-bump
date: 2026-10-30
status: active
module: packages/daemon/internal/ghautomerge
supersedes:
  - docs/solutions/github/a-credentialed-release-job-splits-into-a-build-half-that-hands-over-bytes-and-a-publish-half-that-re-derives-names-recomputes-the-version-and-pushes-one-checked-bump-LEGION-748.md
  - docs/solutions/legion/worker-pane-shell-gotchas.md
related_issues:
  - "sjawhar/legion#2562"
---

# GitHub's GraphQL merge message inputs: an empty body is a value, an empty headline is an omission, and an arm freezes both

Two sentences of the superseded files describe the squash body of a merge Legion makes, and both
are now wrong; everything else in each file stands. LEGION-748's sixth numbered point says "With
squash merges the title becomes `main`'s subject and the commit subjects its body, and
`release-bump.sh` reads both". `worker-pane-shell-gotchas.md` says "the merge queue's squash body
is its READY packet, not the branch's commit subjects". A merge the Legion daemon submits now
carries a body the daemon wrote, and that body is empty, so neither the branch's commit subjects
nor anything else reaches the base branch through it. Each file's conclusion about the release
level — that the pull request title's conventional-commit type is what classifies the bump — is
right, and now right for this reason.

## The rules

- `mergePullRequest` and `enablePullRequestAutoMerge` both take `commitHeadline` and `commitBody`.
  Send both whenever you merge on someone's behalf. With neither, GitHub writes the message its
  `squash_merge_commit_title` and `squash_merge_commit_message` settings name, and the
  `COMMIT_MESSAGES` default joins every commit message of the branch — trailers, session ids,
  skip-check lines and issue keys included — onto the base branch.
- **Measure each field's reading of the empty string.** GitHub documents only the default for an
  *omitted* input ("if omitted, a default message will be used"). The empty string is a third
  state and the two fields do not agree on it: `commitBody: ""` is read as a value and leaves the
  body empty, while `commitHeadline: ""` is read as an omission and GitHub writes its own default
  title. Never infer one field's reading from the other's.
- **GitHub does not edit a headline you supply.** It appends no `(#<number>)` of its own, so the
  caller writes the number into the headline; it truncates nothing; and it escapes nothing. Quotes,
  backticks, `%`, `$`, a `#1234` reference and a trailing `(#9999)` all came back byte for byte.
- **On a base that requires a merge queue, both inputs are ignored** — `EnablePullRequestAutoMergeInput`'s
  own schema note says so — and `EnqueuePullRequestInput` has no message field at all. No change to
  a client reaches that path; only a repository setting does. A claim that a change covers "both
  mutations" is therefore not a claim that it covers every merge: qualify it per code path the API
  has.
- **An arm freezes every input at submission, not only the head.** `enablePullRequestAutoMerge`
  stores the headline and the body when it is accepted and GitHub merges later, so a title edited
  after the arm never reaches the merge — including a title edited to satisfy a check that refused
  the old one. Guarding the head with `expectedHeadOid` does not guard the message.
- **With an empty body, only the pull request's title classifies the release level.** A breaking
  change marked by a `BREAKING CHANGE:` footer in a branch commit body no longer bumps the major
  version through a squash merge the caller writes the message for; a `type!:` title still does.
  Say so wherever prose tells authors the squash body is a fallback.

## Evidence

Measured on sjawhar/legion itself, on scratch base branches of `main`, by two roles with two
different GitHub Apps, with the mutation text read out of the shipped
`packages/daemon/internal/ghautomerge/ghautomerge.go`. The repository's live settings were
`squash_merge_commit_message=COMMIT_MESSAGES`, `squash_merge_commit_title=COMMIT_OR_PR_TITLE`.

- `commitBody: ""`: pull request #2563 merged as `029ce0f390d1` and #2565 as `31085ad1`, each
  message one subject line and nothing else (76 bytes for the second). The control, the same
  mutation as it stood before the change with no message fields, merged #2564 as `3bc250a047b1`
  and #2566 as `013168af4398`, each carrying the joined default body: both branch commit subjects
  as `* ` bullets, both `Omp-Session:` lines, the `skip-checks: true` line, the `BREAKING CHANGE:`
  footer, the issue key and a `Co-authored-by:` trailer.
- `commitHeadline: ""` with `commitBody: ""`: #2568 merged as `77ebcd59`, whose message is
  GitHub's own default title and no body.
- No editing of a supplied headline: #2567 merged as `dd56b30c95d5`, 242 bytes on one line,
  byte for byte the headline sent, its trailing `(#9999) (#2567)` included.
- The release level: `.github/scripts/release-bump.sh 1.0.0 <base>..<merge> -- <path>` printed
  `1.0.1` for the daemon-written merge `31085ad1` (a `fix(scratch):` title, empty body) and
  `2.0.0` for `013168af` (the same two branch commits, the joined body, the `BREAKING CHANGE:`
  footer).
- The arm's frozen inputs: `enablePullRequestAutoMerge` on sjawhar/legion#2562 stored
  `commitHeadline`, `commitBody` and `mergeMethod`, read back with
  `autoMergeRequest { commitHeadline commitBody mergeMethod }`.

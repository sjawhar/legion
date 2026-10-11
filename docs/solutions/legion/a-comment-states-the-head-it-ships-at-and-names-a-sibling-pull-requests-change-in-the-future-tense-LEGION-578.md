---
title: "A comment states the head it ships at and names a sibling pull request's change in the future tense, even when the forward wording was asked for"
category: legion
tags:
  - concurrent-trees
  - sibling-pull-request
  - worker-image
  - dockerfile-comments
  - review-rounds
date: 2026-10-08
status: active
module: packages/daemon/docker, docs
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
  - "LEGION-629"
  - "sjawhar/legion#1848"
---

# A comment states the head it ships at and names a sibling pull request's change in the future tense

- Two trees editing one file split its hunks between them; the one that owns the prose around a
  sibling's hunk is asked to word it "for what holds once both merge". Write it as what holds at
  this head, with the sibling's change in a clause that names its issue and pull request in the
  future tense: `until LEGION-629 (#1848), whose pod lane loads profile plugins: …`, `once the pod
  lane loads profile plugins (LEGION-629), the line also carries extensions=discovered`.
- The test is greppability: a comment that quotes a literal — an OK line, a flag, a path, a step's
  effect — must match this head's source and build output. A quoted mark the binary does not print,
  or "never linked" above a `RUN` that links, is a contradiction the tester and reviewer each file,
  and it costs a round.
- Authority does not change the rule. The forward-looking wording here came from the architect, to
  keep the sibling's hunk conflict-free; the future-tense form is as conflict-free and true on both
  heads.

## Evidence

sjawhar/legion#1846 owned `packages/daemon/docker/worker.Dockerfile`'s header and publish-gate
comments; sjawhar/legion#1848 (LEGION-629, on another branch) owned the plugin-install step, where
it unlinks the two Legion plugins from the profile and adds an `extensions=discovered` mark to the
probe's OK line. Asked to word the comments for both landing, the round-1 push (57cf6a62) stated
LEGION-629's lane as fact — plugins "never linked into the profile", "discovery on", an OK line
quoting `extensions=discovered` — while the same file's step 3 still ran both `omp plugin install`
lines, the probe ran `--no-extensions`, and the Worker Image run at that head printed no such mark.
The tester's `documentationFeedback` and the reviewer's round-2 thread both named it; the round-2
push (4dcac7c5) reworded every sentence to this head's lane with the sibling's change in the future
tense, and the architect's own round-3 note asked for exactly that form.

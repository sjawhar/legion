---
title: "Delete a live proof's scratch fixture as retro's first act"
category: testing
tags:
  - scratch-branch
  - live-proof
  - retro
  - cleanup
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/githubapp
related_issues:
  - "INFRA-390"
  - "sjawhar/legion#2202"
---

# Delete a live proof's scratch fixture as retro's first act

Extends docs/solutions/testing/prove-a-size-dependent-github-read-on-a-scratch-branch-and-drive-dispatch-against-real-github-through-a-loopback-app-proxy-INFRA-390.md.

- Delete the scratch ref, check that GitHub answers 404 for it, and post the deletion comment on
  the pull request before you write any learning. A send-back can end a retro at any point, and
  the next round's proofs re-point the ref anyway.
- Write the pull request body's fixture line in the past tense only for a deletion that happened.
  "deleted again at the next retro" reads as done.

## Evidence

- The ref `scratch/infra-390-large-head` was re-pointed at 43aeb35d at 18:15:18Z for round 2
  (sjawhar/legion#2202 comment 6401406913). The second retro committed the "delete it again at
  the next retro" sentence (a59bd7dd) and was sent back about five minutes in, before it deleted
  the ref. It stayed until the third retro deleted it at 23:22:32Z, after the architect asked
  (comment 6404830309).

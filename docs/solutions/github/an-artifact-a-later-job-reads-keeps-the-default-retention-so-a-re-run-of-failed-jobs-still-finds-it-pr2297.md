---
title: "An artifact a later job reads keeps the default retention, so a re-run of failed jobs still finds it"
category: github
tags:
  - github-actions
  - artifacts
  - retention
  - rerun
  - matrix
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2297"
---

# An artifact a later job reads keeps the default retention, so a re-run of failed jobs still finds it

1. Do not set a short `retention-days` on an artifact that another job of the same workflow
   downloads. "Re-run failed jobs" runs only the failed jobs and reads the other jobs' artifacts
   from the earlier attempt; once those have expired, the re-run fails, and only a full re-run
   recovers, which builds and publishes new outputs.
2. Small handover artifacts (a digest, a version, a file list) cost nothing to keep. Leave them
   at the repository's default retention.
3. Shorten retention only for an artifact that nothing in the workflow reads after its run.

## Evidence

- In `.github/workflows/release-envoy-listener.yaml`, each `docker (<arch>)` leg uploads its
  image digest as `envoy-image-digest-<arch>`, and `image_index` downloads both. With
  `retention-days: 1`, a re-run of `image_index` (or of one failed leg) more than a day later
  stops at `expected 2 digests from the docker legs, found …` (`:327`): review thread
  PRRT_kwDORFy7ds6wucBY on sjawhar/legion#2297.
- The fix leaves the default (`:278-284`); the digest artifacts of run 40546893651 expire on
  2027-01-25.

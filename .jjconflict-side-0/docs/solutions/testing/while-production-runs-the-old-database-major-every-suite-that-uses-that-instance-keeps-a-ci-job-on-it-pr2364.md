---
title: "While production runs the old database major, every suite that uses that instance keeps a CI job on it"
category: testing
tags:
  - ci
  - postgres
  - version-upgrade
  - github-actions
date: 2026-10-28
status: active
module: .github/workflows
---

# While production runs the old database major, every suite that uses that instance keeps a CI job on it

- Before you move CI's database image to a new major, list every database on the production
  instance that still runs the old one, and keep one CI job per suite that uses any of them on
  the old image's digest. Moving CI alone lets a change that needs the new major pass CI and fail
  its deploy.
- Name each job and its comment by the condition that ends it ("… (until production runs Postgres
  18)", "delete this job once production runs Postgres 18"), never by an issue key or link: the
  repository is public, and the condition is what a reader can check.
- Give every database job a step that prints the server's own `server_version_num` (and any
  extension version the suite needs) from the service container, so the run shows which major the
  tests ran on.
- Say in the pull request whether these jobs are required checks. A job that is not required turns
  red on a change that needs the new major, but does not block its merge.

## Evidence

- sjawhar/legion#2364 moved every CI Postgres service to pgvector on 18. The first version kept
  only `dispatch-go-postgres-16` on the 16 digest; review found that the same production instance
  holds the secrets broker's and the Legion daemon's databases, and commit e365a3b4 added
  `broker-go-postgres-16` (`.github/workflows/envoy-and-contracts.yaml`) and
  `daemon-go-postgres-16` (`.github/workflows/pr-and-main.yaml`, commented "Not a required
  check").
- Commit c7f751fc renamed all three from an issue key to "(until production runs Postgres 18)".
- The `Postgres service version` step prints `180006|0.8.7` in `envoy-go` and `160015|0.8.7` in the
  16 jobs.

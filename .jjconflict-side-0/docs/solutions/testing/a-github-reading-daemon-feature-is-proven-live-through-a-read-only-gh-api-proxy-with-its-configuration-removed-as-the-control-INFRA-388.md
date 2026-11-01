---
title: "A GitHub-reading daemon feature is proven live through a read-only gh api proxy, with its configuration removed as the control"
category: testing
tags:
  - live-proof
  - github-api
  - gh
  - envoy
  - daemon-rig
date: 2026-10-25
status: active
module: packages/daemon
related_issues:
  - "INFRA-388"
  - "sjawhar/legion#2203"
---

# A GitHub-reading daemon feature is proven live through a read-only gh api proxy, with its configuration removed as the control

- Boot the real daemon the way a rig does (`check_wake_test.go`'s `run`: real Postgres, NATS
  JetStream, the durable intake consumer, the outbox). Point its `githubAPI` at an `httptest`
  server that answers each GET by running `gh api --include --method GET <path?query>` and
  passing on gh's status, headers and body.
  - Rewrite a `Link` header's `https://api.github.com` to the proxy's origin: `githubrest`
    follows a next page only on its own API origin.
  - Answer every non-GET 405, and log every read.
  - The worker never holds a token, and the daemon reads real api.github.com.
- Build each `source: github` envelope from a real payload with Envoy's own
  `contracts.GithubEnvelopes` (or copy its golden's shape), never by hand. Envoy publishes no
  `workflow_run` whose `pull_requests` list is non-empty (`normalize.go`), so a hand-made
  envelope can prove a path that production never sends.
- The negative control removes only the feature's configuration (here `LIVEPROOF_NO_DEPLOY=1`
  dropped `projects.<KEY>.deploys`). The same envelope must then queue no row and make no GitHub
  read; the proxy's log shows the read count.
- Forgery controls change one claim of a real run at a time: the workflow file, the branch, a
  run id GitHub answers 404, the status.
- Keep the driver behind `//go:build liveproof` under `$TMPDIR` and copy it in only to run it.
  Its own discovery reads (paging run lists) can hit a GitHub 504. That is the driver's read and
  not the daemon's, so rerun it.

## Evidence

- #2203's proof comments: issuecomment-6401192305 and issuecomment-6401553064. Real
  `release.yaml` runs woke looks 2 to 4 on #2182 and look 2 on #2018; with the deploy removed,
  nothing was queued and no GitHub read was made.
- The same driver found the re-run kept-jobs shape that the plan and every unit test had missed
  (docs/solutions/github-api/a-re-run-attempt-lists-the-jobs-it-kept-under-new-ids-with-old-step-times-so-tell-them-by-run-started-at-INFRA-388.md).
- The tester's forgery controls on #2203: a `pr-and-main.yaml` run presented as `release.yaml`,
  a 404 run, and a `pull_request` run claimed on `main`. Each was refused by name.

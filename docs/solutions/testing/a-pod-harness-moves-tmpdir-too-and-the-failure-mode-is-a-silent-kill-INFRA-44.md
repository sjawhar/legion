---
title: "A pod browser harness moves TMPDIR too, and the failure mode is a silent kill with no error naming it"
category: testing
tags:
  - issue-pod
  - playwright
  - postgres
  - tmpdir
  - ephemeral-storage
  - harness
date: 2026-10-20
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-44"
  - "sjawhar/legion#2083"
---

# A pod browser harness moves TMPDIR too, and the failure mode is a silent kill with no error naming it

Extends docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md.

That document says to build the harness on `/legion` and to move `GOCACHE`, `GOMODCACHE` and
`PLAYWRIGHT_BROWSERS_PATH` with it. Two deltas, both learned by paying for them.

- **Read the failure mode first, because the pod will not tell you.** Putting Postgres, the
  browser download or scratch under `/tmp` does not fail with a disk message, a quota error or
  anything naming storage. The process simply dies mid-work and the agent's turn ends; the next
  turn finds a container whose filesystem has been reset, and the obvious reading — a flaky
  command, a slow network, a timeout — is wrong every time. Two downloads were killed this way
  before everything moved onto the issue volume. If a long command in a worker pod dies with no
  output and no status, suspect the container filesystem before anything else.
- **`TMPDIR` is a fourth variable, not covered by the other three.** `apt-get download`,
  `dpkg-deb -x` and the Playwright browser install all stage through `TMPDIR`, so a harness whose
  caches are on the volume still writes a gigabyte under `/tmp` unless `TMPDIR` is exported onto
  the volume as well. Export it in the same sourced env file as the rest.

The same applies to any long-lived cache a role sets up for itself, not only this harness: this
change also had to point `GOMODCACHE` and `GOCACHE` at the volume before a cold `go build` of the
Dispatch server could finish at all, because each container reset discarded the partial module
download and the next attempt started over.

## Evidence

INFRA-44's tester stood the whole stack up inside its own worker pod with everything on
`/legion` — PostgreSQL 17 with pgvector 0.8.0, `bun run build:web`,
`packages/dispatch/e2e/run-server.sh`, real Chromium — and got two full rounds green: round 1,
159 passed across seven specs; round 2, **299 passed, 21 skipped, 0 failed in 16.1 minutes**
across thirteen specs in chromium and iphone, plus seventeen throwaway fault-injection probes.
That is the measurement that made the implementer's `E2E (implementer)` line's premise false, and
it is why this is worth the two lines above: the recipe is cheap once the failure mode is not
mistaken for flakiness.

---
title: "A live proof that switches what a base requires answers every read of the required-checks pass"
category: testing
tags:
  - live-proof
  - required-checks
  - github-stand-in
  - whole-daemon
  - e2e
date: 2026-10-26
status: active
module: packages/daemon/internal/daemon
related_issues:
  - "INFRA-272"
  - "sjawhar/legion#2223"
---

# A live proof that switches what a base requires answers every read of the required-checks pass

Extends docs/solutions/testing/a-github-reading-daemon-feature-is-proven-live-through-a-read-only-gh-api-proxy-with-its-configuration-removed-as-the-control-INFRA-388.md.

A read-only proxy to real GitHub cannot change what a base branch requires during the run. When
the proof needs that change (nothing required, then a check required), drive the whole daemon
against a REST stand-in you control, on the shared harness (`scripts/e2e/lib/live-daemon-proof.sh`,
`packages/daemon/internal/daemon/live_proof_test.go`):

- **Answer every read of the pass with a 2xx.** `requiredchecks.Required` turns any non-2xx into an
  error, and `watchRequiredChecks` then keeps the set it read last. A switch then never reaches the
  daemon, and nothing in the state says so. The reads are:
  - `GET /repos/<o>/<r>/pulls/<n>`, with `base.ref`, `head` and `mergeable: true`;
  - `GET …/pulls/<n>/reviews` (`[]`);
  - `GET …/rules/branches/<base>` (`[{"type":"pull_request"}]` for nothing, plus a
    `required_status_checks` rule for a check);
  - `GET …/branches/<base>` (`{"name":"<base>","protected":false}`);
  - for a send-back, the repository root (`default_branch`) and a `POST /graphql`.

  Answer anything else 404, record it, and report it on failure.
- **Give each pull request its own base branch**, so each issue has its own required set to
  switch.
- **Confirm the switch reached the store before you check that something is absent.** The pass
  runs at boot and then every `requiredChecksInterval` (2 minutes). Poll `pull_requests.required`
  with your own loop bounded at 3 minutes: `testwait.Eventually` gives up after 1 minute. Then
  pair the absence with a control that flips the same input and reads red.
- **Run the fake runtime with `InPod = true`.** Otherwise a send-back's implementer start makes the
  daemon run a real `jj git clone` of the stand-in's repository from github.com.

## Evidence

`scripts/e2e/no-required-checks.sh` takes about 6.5 minutes: three switches, each waiting for one
pass. Step 1 shows no `checksVerdict` on a base that requires nothing. Step 2, its control, switches
the same base to require `ci` and reads `red`. Run 1 of the driver, before `InPod`, logged clone
attempts against github.com in `daemon.log`. The stand-in is `noRequiredChecksGitHub` in
`no_required_checks_live_test.go`.

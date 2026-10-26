---
title: "A local-stack rig a pull request adds is held to product standards before handoff"
category: testing
tags:
  - e2e-rig
  - secrets
  - flaky
  - fixtures
  - text-decoder
  - scratch
date: 2026-10-26
status: active
module: scripts/e2e
related_issues:
  - "INFRA-195"
  - "sjawhar/legion#2205"
---

# A local-stack rig a pull request adds is held to product standards before handoff

Four of the six review threads on the pull request that added `scripts/e2e/dispatch-session-settings.sh`
were defects in the rig, not in the product. Check a new rig against this list before the first
handoff.

## Rule

- **Pin a fixture that stands for "before feature X" to an exact version.** `@latest` moves to the
  release that ships X once the change merges, and the rig then fails on correct code.
- **Scope secrets from the top.** Copy each key with `${VAR:-}` (a bare `${#VAR}` exits under
  `set -u` when the key is unset) and `unset` it before the script starts anything; the transcript's
  `tee` is a child too. Hand the keys back only on the driver's command, which deletes them from its
  own `process.env` at once (Playwright's browser inherits it) and passes them only to the process
  that needs them. Prove it with a sampler of `/proc/<pid>/environ` that prints variable names only.
- **Give every concurrent stream reader its own `TextDecoder`.** In stream mode a decoder holds a
  character split across chunks; one shared between readers prefixes another stream's line with
  `\uFFFD`, the line fails `JSON.parse`, and a reader that drops unparseable lines turns a lost RPC
  answer into a timeout in some unrelated check.
- **Wait for the record the server writes second.** Read the server's commit order before judging
  a record: Dispatch commits a message, then records its delivery attempt (`createMessage`,
  `deliverMessage`), so a read in between holds the message and no attempt. Poll until the
  dependent record exists. Prove the wait with a scratch `time.Sleep` between the server's two
  steps: the old driver fails, the new one passes.
- **Run the engines the pull request claims.** `--list` shows a row is selected, not that it
  passes; Firefox and WebKit launch in an issue pod
  (docs/solutions/testing/webkit-and-firefox-launch-rootless-in-an-issue-pod-from-unpacked-debs-a-patched-minibrowser-and-an-egl-vendor-dir-INFRA-442.md).
- **Write the driver as modules and a table of named checks** from the start, so a check the
  environment cannot satisfy can be run last or left out by name, and say in the proof which checks
  ran, on which inputs, and where the left-out one last passed.
- **A fallback check needs a fallback the host takes.** Oh My Pi skips a same-provider fallback
  candidate after a turn carrying another model of that provider's signed thinking
  (dispatch://INFRA-309); a fallback check on two Anthropic models retries and never falls back.
  Keep a cross-provider pair as the default.
- **Keep evidence where no subagent works.** Give each subagent a scratch directory of its own and
  keep the run's evidence outside every path a subagent is handed; say in its brief that it deletes
  nothing it did not create.

## Evidence

- Pinned fixture: review round 1 (thread PRRT_kwDORFy7ds6wG368); `older_pi_envoy=10.24.0` in
  779874f4.
- Secrets: threads PRRT_kwDORFy7ds6wG369 (`set -u`) and PRRT_kwDORFy7ds6wG37A (keys reached the
  builds, nats-server and the listener). The first fix unset the keys after setup, and the sampler
  still caught the transcript's `tee` holding them; 87ea738e moved the unset to the script's top.
- Decoder: thread PRRT_kwDORFy7ds6wLDs8. A probe split "é" across one stream's chunks while a
  second stream and a NATS message used the shared decoder: the second line read
  `\uFFFD{"type":"ready"}` and failed to parse; one decoder per reader (fbd76fa8) fixed it.
- Second-step record: the tester's run failed `FAIL no-answer: Dispatch's send failed: null` while
  the page showed the switch delivered; with a scratch 2 s sleep before `deliverMessage` the old
  driver failed every time and the fixed one (2451b5dc) passed all 20 checks.
- Engines: the first round recorded only `--list` for Firefox and WebKit; CI's Firefox project then
  failed two rows two rounds later
  (docs/solutions/envoy/an-event-stream-writes-a-byte-with-its-headers-because-firefox-opens-a-fetch-only-on-its-first-body-byte-INFRA-195.md).
- Table of checks: a 2,045-line driver with one 1,060-line `run()` drew a code-quality finding;
  88980493 split it. Later rounds moved `fallback` last by a scratch edit to run checks 19 and 20.
- Fallback: the check passed on anthropic/claude-haiku-5-5 with openai/gpt-5-nano and failed on two
  Anthropic models with the old and new driver alike; the Oh My Pi fork's owner reproduced it from
  dispatch://INFRA-195/artifact/check18-fallback-log-txt.
- Scratch: two subagents each removed a scratch directory under `$TMPDIR` that held the
  orchestrator's evidence (one called it its own), and the runs were repeated.

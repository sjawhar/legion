---
title: "Playwright gets OS-chosen ports from a wrapper that starts the servers before it runs"
category: testing
tags:
  - playwright
  - ports
  - test-harness
  - argv-parsing
date: 2026-10-27
status: active
module: packages/dispatch
related_issues:
  - "INFRA-253"
  - "sjawhar/legion#2229"
---

# Playwright gets OS-chosen ports from a wrapper that starts the servers before it runs

Extends docs/solutions/testing/dispatch-playwright-harness-from-a-legion-pane-the-harness-owns-its-envoy-own-your-ports-database-and-paths.md.

## Rule

- Playwright evaluates its config in the runner, again in every worker, and in its loader
  process, and `webServer` needs each port when the config evaluates. It cannot learn a port a
  server chose. Start the servers in a wrapper (`packages/dispatch/e2e/run.ts`), read each one's
  reported port, export them, then spawn Playwright; the config starts no server.
- A module that a harness starter imports before the ports exist reads them in a function called
  at use time, not in a module-level constant, or it keeps the zeros it saw.
- A bare run with the ports unset refuses at config evaluation, naming the wrapper. A fallback that
  starts servers itself brings the port race back.
- An argv test for `--list` is loose: Playwright reads `--list` after `--grep` or `--grep-invert` as
  that option's value, which is a real run. Exempt a listing only from a refusal that guards
  nothing; keep every refusal that protects data (here, the one against running on servers whose
  database `seed.ts` truncates) whenever a targeted port is set.

## Evidence

- `packages/dispatch/e2e/run.ts`, `harness-start.ts`, `harness-ports.ts` (`harnessPorts()` became
  a function), `playwright.config.ts`.
- Review thread PRRT_kwDORFy7ds6wQfCf: with a port set and no `DISPATCH_E2E_REUSE_SERVERS`,
  `--grep-invert --list` passed the reuse refusal and ran specs; fixed in cee45e9b89b6. With the
  fix, a bare `--list` still lists and `FAKE_ENVOY_PORT=<n> … --grep-invert --list` is refused.

---
title: "A proof that an endpoint accepts a key also proves it refuses a keyless call, and names the key by its alias"
category: testing
tags:
  - live-proof
  - model-gateway
  - litellm
  - negative-control
date: 2026-10-29
status: active
module: scripts/e2e
related_issues:
  - "sjawhar/legion#2186"
---

# A proof that an endpoint accepts a key also proves it refuses a keyless call, and names the key by its alias

- A 200 to your key proves nothing if the endpoint answers anyone. Also send the same request
  with no key and fail the run when it answers 200; note the code it refused with.
- Name the key the endpoint saw, by its non-secret alias, in the run's transcript, so a run can
  never pass on another key unnoticed. A LiteLLM proxy serves key information at its root,
  `GET /key/info` (`key_alias` at the top level or under `info`); `/v1/key/info` is not found.
  A missing alias is a note, never a failure.
- Keep the key out of argv and logs: a 0600 header file for curl, and treat the key-info body as
  secret too, since it may echo the key.
- Take the endpoint as an operator input (a base URL), never a literal, and let the devbox's
  preflight use its own address when the pods reach the endpoint at a cluster-only one.

## Evidence

`scripts/e2e/lib/model-endpoint.sh` (`model_key_preflight`), sourced by both Claude Code live
proofs. Its first version asked `/v1/key/info`; two passing runs through the gateway each noted
that the endpoint named no alias, which a red test then pinned (fixed in commit 62102f1a).

---
title: "A default provider the pod holds no credential for is proven to change nothing, and is named unproven live"
category: testing
tags:
  - dispatch
  - embeddings
  - bedrock
  - live-proof
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/embed
related_issues:
  - "INFRA-393"
  - "sjawhar/legion#2208"
---

# A default provider the pod holds no credential for is proven to change nothing, and is named unproven live

- When a change adds a second model provider and refactors the default one, and the issue pod
  holds credentials for the new provider only, the live proof of the default path can show what
  it leaves alone, never that it works.
- Drive the default path with no credential and assert what must not happen: no row dropped or
  re-admitted, no call made when nothing is foreign, the same identity on every stored vector.
- Write in the PR body, by name, what no run reached: a successful call on the default provider,
  its canary succeeding, a fresh vector ranked, and a switch back from the new provider. Leave
  the rest to the unit tests and the production check, not to an inference from the new
  provider's runs.
- If the default path's working matters before the merge, ask for the credential (a `dispatch
  ask`) at planning. Do not discover the gap at review.

## Evidence

INFRA-393 refactored Bedrock (`embed.New` became `embed.NewBedrock`, behind `embed.Embedder`'s new
`Identity` and `Limits`) and added Azure AI Foundry. The issue pod reached Azure keyless, through
its projected token exchanged at Entra, and held no AWS credential. Every Bedrock run booted with
`AWS_REGION` and no credential. It showed the identity `us.cohere.embed-v4:0` matching every
vector an earlier release stored, `foreignDeadReadmitted=0` with no call, and a failing canary
dropping nothing. The round-3 review recorded that no Bedrock call answered, and the fresh-eyes
retro named the four results above as unproven live. "Bedrock unchanged" rests on those runs, on
`bedrock_test.go`'s request-shape tests, and on the production check after the deploy.

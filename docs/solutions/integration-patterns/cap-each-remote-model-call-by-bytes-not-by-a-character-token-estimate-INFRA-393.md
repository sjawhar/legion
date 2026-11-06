---
title: "Cap each remote model call by bytes, not by a character-based token estimate"
category: integration-patterns
tags:
  - dispatch
  - embeddings
  - azure
  - timeouts
  - cjk
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/embed
related_issues:
  - "INFRA-393"
  - "sjawhar/legion#2208"
---

# Cap each remote model call by bytes, not by a character-based token estimate

- When a model service's request timeout can be shorter than its time to process a full batch,
  bound each call by the UTF-8 bytes it carries. A token is at least one byte, so a byte cap is a
  hard ceiling on tokens. Set the cap from the slowest throughput measured on the real service,
  with room to spare (`embed.Limits.MaxBatchBytes`, closed by `embedqueue`'s `chunkForBudget`).
- Never size a call by a characters-per-token estimate. Four characters a token
  (`embed.EstimateTokens`) under-counts hex and dense CJK about 3.5 times, and random CJK more.
- Prove it on the real service with a dense seed, by counting each committed call's bodies and
  bytes. Embedded rows sharing one `embedded_at` value came from one call.
- The estimate still paces the per-minute token budget, so dense text can pass the quota. The
  service then answers 429, which the queue must retry as a throttle and never count as a failure.

## Evidence

Azure AI Foundry's Cohere Embed v4 accepts 96 texts per call (live: 97 is refused), and
INFRA-393's client times out at 60 s. One direct call of 40 dense CJK bodies (1,049,910 tokens)
took 65 s. Through the server, 50 pending CJK comments went as one 4,799,720-byte call: it timed
out, was bisected, and still spent its tokens. A 32,000-character CJK body is 26,255 tokens by
Azure's count, against an estimate of 8,000.

With `azureMaxBatchBytes = 320_000` (`embed/azure.go`; about 20 s at the slowest rate measured),
the same 50 comments went in 17 calls of at most 3 bodies and 287,988 bytes. All became current
with no retry and no dead row. Bedrock sets no byte budget (`MaxBatchBytes` 0), so its batches are
unchanged.

---
title: Meaning search embedding queue
parent: dispatch-server
depends_on: [dispatch-duty, dispatch-model, dispatch-store, dispatch-text]
paths: [packages/envoy/internal/dispatch/embedqueue, packages/envoy/internal/dispatch/embed]
---
How meaning search's vectors are made and kept. `embed` is the embedder client, Cohere Embed v4 on AWS Bedrock or Azure AI Foundry, whose identity every stored vector records. `embedqueue` embeds what the write triggers enqueue in `embeddings`: a woken loop (`duty.RunWoken`) that scans when `embeddings_enqueue` notifies `dispatch_embeddings` (migration 0150), when the coverage pass writes rows, or when the earliest pending row falls due, at most once per 5 s, claiming its rows so two processes never embed the same one, and never calling the model inside a write's transaction. The coverage pass (`Cover`) runs once at start with no lease: it adds what no trigger saw and, once the embedder has answered, drops vectors another identity made; only those identity steps run again, while such rows remain. `GET /api/v1/search/coverage` reads its counts (`Coverage`).

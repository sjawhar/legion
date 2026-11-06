---
title: "A switch to a new model service drops the old one's state only after the new one answers"
category: integration-patterns
tags:
  - dispatch
  - embeddings
  - configuration
  - canary
  - azure
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/embedqueue
related_issues:
  - "INFRA-393"
  - "sjawhar/legion#2208"
---

# A switch to a new model service drops the old one's state only after the new one answers

- A step that deletes or invalidates what an earlier configuration produced (another embedder's
  vectors, rows dead under another model) runs only after proof that the new configuration
  works: a real call by this process that succeeded, or a canary call that succeeds.
- Until then it changes nothing, logs one WARN naming the configured identity, the canary's error
  and how many rows it kept, and tries again on the next pass. A failed or throttled canary proves
  nothing.
- Prove it live with a configuration that is wrong but well-formed: a misnamed deployment, a wrong
  key, a wrong client id. Boot checks cannot catch any of these, and each is what an operator
  types by mistake.

## Evidence

INFRA-393's plan had the coverage pass drop every vector of another identity at its first pass.
The tester booted with `DISPATCH_AZURE_EMBED_DEPLOYMENT=embed-v-4-0-typo`. The pass logged
`foreignVectorsDropped=41` before Azure answered `404 DeploymentNotFound`, which left those items
with no vector at all. Reverting re-embedded them all, at a cost in time and tokens.

`decideIdentitySteps` (`embedqueue/coverage.go`) now runs the drop and the re-admission only once
the queue has embedded something (`Deps.proven`) or a canary succeeds. Live, the misnamed
deployment logged `coverage pass left other identities' vectors and dead rows in place …
foreignVectorsLeft=94 foreignDeadLeft=1` and kept every row. The right deployment on the same
database then dropped the one foreign vector and re-admitted the one foreign dead row. The tester
also showed it with a wrong key, a wrong keyless client id, `AWS_REGION` with no credential, and a
healthy process and a misnamed one on the same database.

Review weighed an alternative: keep foreign vectors and only mark their rows pending. It kept the
gate, since dropping keeps foreign vectors out of the HNSW index, which search filters by model.

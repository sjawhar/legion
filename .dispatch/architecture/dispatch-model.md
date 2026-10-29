---
title: Shared domain model
parent: dispatch-server
depends_on: [dispatch-auth, dispatch-store, envoy-listener]
paths: [packages/envoy/internal/dispatch/model, packages/envoy/internal/dispatch/asks, packages/envoy/internal/dispatch/rank, packages/envoy/internal/dispatch/identity]
---
The row and read shapes the API and the document service share (`model`, a search hit and the answer state of an ask hit among them), plus three small packages that travel with them: ask-follower bookkeeping (`asks`), fractional rank allocation (`rank`), and actor identity resolution with the sign-in pool's membership check, which names every person by lowercase email (`identity`, which imports `auth` and the listener's `oidc`; a lookup the server could not serve, because the read passed its deadline, the client went away or no database connection came free in time, it answers as the store's `admission` answers it, a retryable `503` or nothing, rather than as an identity failure). None is large enough to be its own component, and the bugs they produce — a rank collision, a missed follower, an ask read through the wrong columns — surface across every caller at once.

`model` carries the related-item check's result (`Suggestions`: its outcome, how it searched, the search words, the related items with each ask's answer state as `SearchAsk`, and the title matches under `--force`) and the records route's report (`RelatedChecksReport`).

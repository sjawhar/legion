---
title: Shared domain model
parent: dispatch-server
depends_on: [dispatch-auth]
paths: [packages/envoy/internal/dispatch/model, packages/envoy/internal/dispatch/asks, packages/envoy/internal/dispatch/rank, packages/envoy/internal/dispatch/identity]
---
The row and read shapes the API and the document service share (`model`), plus three small packages that travel with them: ask-follower bookkeeping (`asks`), fractional rank allocation (`rank`), and actor identity resolution (`identity`, which imports `auth`). None is large enough to be its own component, and the bugs they produce — a rank collision, a missed follower, an ask read through the wrong columns — surface across every caller at once.

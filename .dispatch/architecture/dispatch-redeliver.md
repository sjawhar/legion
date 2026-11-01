---
title: GitHub webhook redelivery
parent: dispatch-server
depends_on: [envoy-listener, dispatch-auth, dispatch-duty]
paths: [packages/envoy/internal/dispatch/redeliver]
---
The sweep that asks GitHub to redeliver the App webhook's failed deliveries, which GitHub never redelivers itself. At start and then every two minutes, each wait the interval plus up to a tenth again drawn afresh every period (`duty.Jittered`), under the `webhook-redelivery` lease (`duty.Exclusive`, once per period across the processes), it lists the webhook's failed attempts as the App (`dispatch-auth`), judges each delivery by its GUID, and requests a redelivery of each the listener answered 5xx or GitHub could not complete, with a doubling backoff, at most five times. Its cursor, the last rate limit and one record per delivery acted on live in the JetStream KV bucket `envoy_webhook_redelivery`, reached through the listener's bus client (`envoy-listener`). `envoy-dispatch redeliver-webhooks` runs the same sweep over a chosen window.

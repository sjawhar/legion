---
title: Delivery timeline intake and reconcile
parent: dispatch-server
depends_on: [contracts, envoy-listener, dispatch-auth, dispatch-duty, dispatch-events, dispatch-model, dispatch-store]
paths: [packages/envoy/internal/dispatch/delivery]
---
The Delivery page's record of pull requests, workflow runs and deploys, kept from GitHub two ways. The intakes consume GitHub notifications from NATS on durable consumers (one binder per durable, the other process standing by): `Intake` checks each message's subject before decoding anything, and runs pull requests and workflow runs at once up to the consumer's `MaxAckPending`, in delivery order per pull request or run; `StopDelivering` hands its queued messages back to NATS for the next holder. Both intakes route by one `SettingsCache`, read at start and again when `delivery_settings_changed` notifies `dispatch_delivery_settings` (migration 0154). The reconcile (`Reconcile.Run`) is a five-minute timer loop (`duty.RunJittered`, lease `delivery-reconcile`) that catches up what no webhook delivered, since GitHub has no feed of what was missed; `measures` computes the DORA and pipeline figures from the stored rows. It reaches NATS through the listener's bus client (`envoy-listener`), GitHub through the App client (`dispatch-auth`), and decodes envelopes with the generated Go contracts (`contracts`).

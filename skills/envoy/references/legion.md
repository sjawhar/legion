# Envoy in a Legion pane

Read this in a Legion pane before you touch a role claim, or when a Legion role message fails.

- The Legion plugin claims your assigned role at session start through a daemon-minted role token. Never call `envoy_role_set` and never subscribe to your role topic yourself, because the claim is already made (`packages/pi-shared/src/legion-claim-core.ts`).
- Never build a role token from an issue number or title: only the daemon maps tokens to issues.
- A Legion claim is last-claim-wins. It survives parking and worker re-creation, and stays through the issue's linger after close (the deployment's `linger_hours`).
- A Legion role failure names one of five reasons. Read who published the message before you act, because only the daemon re-sends its own architect notices.
  - `unclaimed`: the listener refused the publish with `404`; no claim names the role, so nothing was sent.
  - `holder_lapsed`: the listener refused the publish with `404`; the claim names a session that is no longer live, so nothing was sent.
  - `no_holder`: the listener accepted the publish, then found no live holder when it forwarded it, so it reached nobody.
  - `delivery_failed`: the listener accepted the publish, but cannot show it reached the holder: the holder lookup failed, the holder was stale, or the forward failed.
  - `receipt_timeout`: the listener forwarded the message to a live holder, but no receipt came in time, so it may have arrived.
- For the daemon's own architect notice, never re-send it by hand. After `no_holder`, `delivery_failed` or `receipt_timeout`, the daemon queues it again, at most three times. After `unclaimed` or `holder_lapsed`, its outbox row retries by itself until the role is held or the architect cannot hold it again.
- A refused publish from anyone except the daemon's outbox is re-sent by no one, and so is a failed delivery of it. After `unclaimed` or `holder_lapsed`, the publisher got the `404` and still has the message; it must act on its own `404` and decide whether to send it again. After a `200`, no tool tells the publisher about `no_holder`, `delivery_failed` or `receipt_timeout`. `envoy_publish` cannot repeat the first dedupe key, so a copy after `receipt_timeout` can arrive twice.
- Never add a second subscriber, because the listener forwards a role message only to its holder.
- Never forward a failure payload to an architect. The daemon handles only its own architect notices, and the publisher must handle its own message.

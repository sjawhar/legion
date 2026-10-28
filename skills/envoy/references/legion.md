# Envoy in a Legion pane

Read this in a Legion pane before you touch a role claim, or when a Legion role message fails.

- The Legion plugin claims your assigned role at session start through a daemon-minted role token. Never call `envoy_role_set` and never subscribe to your role topic yourself, because the claim is already made (`packages/pi-shared/src/legion-claim-core.ts`).
- Never build a role token from an issue number or title: only the daemon maps tokens to issues.
- A Legion claim is last-claim-wins. It survives parking and worker re-creation, and stays through the issue's linger after close (the deployment's `linger_hours`).
- A role publish can fail in two places. Read who published the message before you act, because only the daemon re-sends its own architect notices.
  - The listener refused the publish, so nothing was sent. `envoy_publish` returns an error: `no holder for role <role>` when no claim names the role, or `role <role> holder <session> lapsed; ...` when the claim names a session that is no longer live. The listener also publishes an exception for the refusal, with the reason `unclaimed` or `holder_lapsed`.
  - The listener accepted the publish and `envoy_publish` returned the holder, but the forward failed. Only an exception on `notifications.envoy.exceptions.notifications.role.<role>` reports it, with one of three reasons:
    - `no_holder`: no live session held the role when the listener forwarded the message, so it reached nobody.
    - `delivery_failed`: the listener cannot show that the message reached the holder: the holder lookup failed, the holder was stale, or the forward failed.
    - `receipt_timeout`: the listener forwarded the message to a live holder, but no receipt came in time, so it may have arrived.
- For the daemon's own architect notice, never re-send it by hand. After `no_holder`, `delivery_failed` or `receipt_timeout`, the daemon queues it again, at most three times. After a refusal, its outbox row retries by itself until the role is held or the architect cannot hold it again.
- Nobody re-sends any other message. After a refusal you still have the message: call `envoy_role_get(role="<role>")`, or find the role in `envoy_sessions`, and send the message again when a live session holds the role. Until then, `envoy_role_get` returns one of the two refusal errors above, not a holder.
- After `envoy_publish` returned a holder, no tool tells you that the forward failed. To learn of it, subscribe to `notifications.envoy.exceptions.notifications.role.<role>` before you publish, and match each exception's `event_id` to the `envelope.event_id` that `envoy_publish` returned: that topic is live only, and it carries the failures of every publisher to the role. `envoy_publish` cannot repeat the first dedupe key, so a copy after `receipt_timeout` can arrive twice.
- Never add a second subscriber, because the listener forwards a role message only to its holder.
- Never forward a failure payload to an architect. The daemon handles only its own architect notices, and the publisher must handle its own message.

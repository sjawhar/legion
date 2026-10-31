# Envoy in a Legion pane

Read this in a Legion pane before you touch a role claim, or when a Legion role message fails.

- The Legion plugin claims your assigned role at session start through a daemon-minted role token. Never call `envoy_role_set` and never subscribe to your role topic yourself, because the claim is already made (`packages/pi-shared/src/legion-claim-core.ts`).
- Never build a role token from an issue number or title: only the daemon maps tokens to issues.
- A Legion claim is last-claim-wins. It survives parking and worker re-creation, and stays through the issue's linger after close (the deployment's `linger_hours`).
- `no_holder`, `delivery_failed` and `receipt_timeout` on a Legion role are the daemon's liveness signals. Never add a second subscriber or retry by hand: the daemon revives or recreates the worker and re-delivers, or relaunches the root process, which gets a `catch-up` notice of its tree and reads each issue's handoffs on its Dispatch issue (`dispatch handoff-read`).
  - `no_holder`: no live session holds the role.
  - `delivery_failed`: the message was not forwarded at all: the holder lookup failed, the holder was stale, or the publish errored.
  - `receipt_timeout`: the listener forwarded the message to a live holder and no receipt arrived in time. Its payload keeps `original_topic`, `event_id`, `payload` and the original `dedupe_key`, so the daemon can re-send it and the receiver's dedupe drops a copy that did arrive.
- Never forward these failure payloads to an architect, because the daemon already acts on them.

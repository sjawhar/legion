# Envoy in a Legion pane

Read this in a Legion pane before you touch a role claim, or when a Legion role message fails.

- The Legion plugin claims your assigned role at session start through a daemon-minted role token. Never call `envoy_role_set` and never subscribe to your role topic yourself, because the claim is already made (`packages/pi-shared/src/legion-claim-core.ts`).
- Never build a role token from an issue number or title: only the daemon maps tokens to issues.
- A Legion claim is last-claim-wins. It survives parking and worker re-creation, and stays through the issue's linger after close (the deployment's `linger_hours`).
- `no_holder`, `delivery_failed` and `receipt_timeout` on a Legion role are the daemon's liveness signals. Never add a second subscriber or retry by hand: the daemon revives or recreates the worker and re-delivers, or relaunches the root process, which gets a `catch-up` notice of its tree and reads the handoffs under `.legion/<issue>/`.
  - `no_holder`: no session holds the role's claim. Only the reaper releases a claim, once its holder is `gone`; a Legion role whose agent process exits is `gone` at once, so its successor's soft claim takes the role without waiting out the live window.
  - `delivery_failed`: the message is not known to have reached the holder: the holder lookup failed, the holder's session could not be reached, the publish errored, or the stream did not store the forward. In the last case the holder can have seen the message live, and its dedupe drops a re-sent copy. It releases no claim.
  - `receipt_timeout`: the listener forwarded the message to the holder and no receipt arrived in time. The error names the holder's `holder_state` and `last_seen`. An `unresponsive` holder is not dead: it keeps the role, nothing replaces it, and the message waits for it. Its payload keeps `original_topic`, `event_id`, `payload` and the original `dedupe_key`, so the daemon can re-send it and the receiver's dedupe drops a copy that did arrive.
- A role message forwarded to you is kept on your inbox in the stream, which your own durable consumer reads, so your session gets it once, also after the pane restarts or reconnects.
- Never forward these failure payloads to an architect, because the daemon already acts on them.

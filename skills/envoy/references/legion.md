# Envoy in a Legion pane

Read this in a Legion pane before you touch a role claim, or when a Legion role message fails.

- The Legion plugin claims your assigned role at session start through a daemon-minted role token.
  Never call `envoy_role_set` and never subscribe to your role topic yourself, because the claim is already made (`packages/pi-shared/src/legion-claim-core.ts`).

- Never build a role token from an issue number or title: only the daemon maps tokens to issues.

- A Legion claim is last-claim-wins.
  It survives parking and worker re-creation, and stays through the issue's linger after close (the deployment's `linger_hours`).

- `no_holder`, `holder_lapsed` and `delivery_failed` on a Legion role are the daemon's liveness signals.
  Never add a second subscriber or retry by hand:
  the daemon revives or recreates the worker and re-delivers, or relaunches the root process, which gets a `catch-up` notice of its tree and reads each issue's handoffs on its Dispatch issue (`dispatch handoff-read`).

  - `no_holder`: no session holds the role's claim.
    Only the reaper releases a claim, once its holder is `gone`;
    a Legion role whose agent process exits is `gone` at once, so its successor's soft claim takes the role without waiting out the live window.

  - `holder_lapsed`: a session holds the claim but has not registered within the live window, so the listener refused the publish `404` and forwarded nothing.
    The error names the holder, its `holder_state` and its `last_seen`, and says `claim_released: false`.
    An `unresponsive` holder is not dead: it keeps the role, nothing replaces it, and the daemon holds the packet and publishes it again.

  - `delivery_failed`: the stream stored no forward for the holder:
    the holder lookup failed, the publish errored, or the stream did not store the forward within the send's window.
    Every forward goes into the stream, so a holder that is live but not reading is forwarded to like any other and its own consumer reads the message when it answers again;
    nothing is published live, and a holder cannot have seen a message the stream never stored.
    It releases no claim.

  - No `receipt_timeout`: this release's listener waits for no receipt, so no signal here says a holder was slow.
    A role send answered `200` says that the stream stored the forward for the holder, and a forward the stream did not store is `delivery_failed`.

- A role message forwarded to you is kept on your inbox in the stream, which your own durable consumer reads, so your session gets it once, also after the pane restarts or reconnects.

- Never forward these failure payloads to an architect, because the daemon already acts on them.

- A role send the listener refuses `404` also raises one exception on `notifications.envoy.exceptions.notifications.role.<role>`, with the reason it refused: `unclaimed` or `holder_lapsed`.
  It is a bus signal and nothing more: no consumer acts on a peer's refusal, and the daemon's own refused outbox row retries by itself until the role is held or its architect cannot hold it again.
  Never re-send a refused message by hand on the strength of one.

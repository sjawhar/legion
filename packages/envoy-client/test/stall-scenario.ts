import { expect } from "bun:test";
import { REPLAYED_SUFFIX } from "../src/replay";
import type { StallProxy } from "./stall-proxy";

/** The spec's bound: a session finds a path that answers within a minute of the freeze. */
const RECOVERY_BOUND_MS = 60_000;

/** A session plugin under test, registered and past its first sweep, whose NATS path `proxy` fronts. */
export interface FreezeScenarioHost {
  /** What the timing log line starts with, such as `pi-envoy stall`. */
  readonly label: string;
  /** The listener's base URL. */
  readonly envoyUrl: string;
  /** The session the direct messages go to. */
  readonly sessionID: string;
  /** The proxy the session's only NATS connection goes through. */
  readonly proxy: StallProxy;
  /** The text of every message the host showed the session that holds `marker`, in order. */
  shown(marker: string): readonly string[];
  /** Resolves once `condition` holds; throws, naming `what`, when `deadline` passes first. */
  waitFor(
    condition: () => boolean | Promise<boolean>,
    deadline: number,
    what: string
  ): Promise<void>;
}

/**
 * The freeze every session plugin must survive: a direct message shown live, then the session's
 * NATS path frozen and a second message sent while it is cut off, which must be shown within 60 s
 * of the freeze after the session subscribed again on a new connection, and a third after the
 * reconnect. Each of the three is shown once, and only the one sent during the freeze as replayed.
 *
 * The host runs on nats.js's real ping and dial timers against a real listener, so each wait is
 * real time.
 */
export async function runFreezeScenario(host: FreezeScenarioHost): Promise<void> {
  const { label, envoyUrl, sessionID, proxy, shown, waitFor } = host;
  const send = async (marker: string) => {
    const answer = await fetch(`${envoyUrl}/v1/messages/send`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ target_session: sessionID, source: "agent", message: marker }),
    });
    expect(answer.status).toBe(200);
  };
  const run = crypto.randomUUID();

  const live = `stall-live-before-${run}`;
  await send(live);
  await waitFor(() => shown(live).length > 0, Date.now() + 10_000, "the live message");

  const frozenAt = proxy.freeze();
  const missed = `stall-missed-${run}`;
  await send(missed);
  await waitFor(
    () => shown(missed).length > 0,
    frozenAt + RECOVERY_BOUND_MS,
    "the message sent while the path was frozen"
  );
  const shownAt = Date.now();
  const resubscribed = proxy.connections.find(
    (opened) => opened.acceptedAt >= frozenAt && opened.subscribedAt !== undefined
  );
  console.log(
    `${label}: resubscribed ${(((resubscribed?.subscribedAt ?? Number.NaN) - frozenAt) / 1000).toFixed(1)} s, missed message shown ${((shownAt - frozenAt) / 1000).toFixed(1)} s after the freeze`
  );
  expect(resubscribed?.subscribedAt).toBeDefined();
  expect((resubscribed?.subscribedAt ?? Number.POSITIVE_INFINITY) - frozenAt).toBeLessThan(
    RECOVERY_BOUND_MS
  );
  expect(shownAt - frozenAt).toBeLessThan(RECOVERY_BOUND_MS);

  const after = `stall-live-after-${run}`;
  await send(after);
  await waitFor(
    () => shown(after).length > 0,
    Date.now() + 10_000,
    "the message after the reconnect"
  );

  // Each shown once: the two live without the replay line, the frozen window's replayed.
  for (const [marker, replayed] of [
    [live, false],
    [missed, true],
    [after, false],
  ] as const) {
    const messages = shown(marker);
    expect(messages).toHaveLength(1);
    expect(messages[0]?.includes(REPLAYED_SUFFIX)).toBe(replayed);
  }
}

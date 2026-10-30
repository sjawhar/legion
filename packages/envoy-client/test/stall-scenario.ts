import { expect } from "bun:test";
import { agentSubject } from "@legion/contracts";
import { connect, type KV, type NatsConnection } from "nats";
import { z } from "zod";
import { deliverSubject } from "../src/consumer-delivery";
import { REPLAYED_SUFFIX } from "../src/replay";
import type { EnvoyStack } from "./envoy-stack";
import type { StallProxy } from "./stall-proxy";

/** The spec's bound: a session finds a path that answers within a minute of the freeze. */
const RECOVERY_BOUND_MS = 60_000;

/** A session plugin under test, registered, whose NATS path `proxy` fronts. */
export interface FreezeScenarioHost {
  /** What the timing log line starts with, such as `pi-envoy stall`. */
  readonly label: string;
  /** The natsd and listener the session runs against. */
  readonly stack: EnvoyStack;
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

/** The fields of the listener's session entry (`envoy_sessions`) the scenario reads. */
const SessionEntrySchema = z.object({
  delivery: z.string().optional(),
  epoch: z.string().optional(),
});

/** natsd's `/connz?subs=1`: each connection, when it started and what it subscribes. */
const ConnzSchema = z.object({
  connections: z
    .array(
      z.object({
        start: z.string(),
        subscriptions_list: z.array(z.string()).optional(),
      })
    )
    .optional(),
});

/** The session's entry as the listener stored it; undefined while it has none. */
async function sessionEntry(
  sessions: KV,
  sessionID: string
): Promise<z.infer<typeof SessionEntrySchema> | undefined> {
  const entry = await sessions.get(sessionID);
  if (entry === null || entry.operation !== "PUT") return undefined;
  return SessionEntrySchema.parse(entry.json());
}

/** Whether a connection natsd accepted at or after `since` (`Date.now()` time) subscribes `subject`. */
async function subscribedSince(monitorUrl: string, subject: string, since: number) {
  const response = await fetch(`${monitorUrl}/connz?subs=1`, {
    signal: AbortSignal.timeout(2_000),
  });
  if (!response.ok) return false;
  const connz = ConnzSchema.parse(await response.json());
  return (connz.connections ?? []).some(
    (connection) =>
      Date.parse(connection.start) >= since &&
      (connection.subscriptions_list ?? []).includes(subject)
  );
}

/** The listener's answer to a registration that asked for a session consumer. */
const DeliveryAnswerSchema = z.object({
  delivery: z.object({ mode: z.string(), reason: z.string().optional() }).optional(),
});

/**
 * The delivery a registration asking for a session consumer is answered with now, from a
 * registration of the scenario's own: a session id of its own, so the plugin's own session is left
 * alone, removed again once it is answered.
 */
async function registrationDelivery(
  envoyUrl: string
): Promise<z.infer<typeof DeliveryAnswerSchema>["delivery"]> {
  const probe = `ses_grantprobe${crypto.randomUUID().replaceAll("-", "")}`;
  const answer = await fetch(`${envoyUrl}/v1/interests/subscribe`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      session_id: probe,
      self_subscribed: true,
      topics: [],
      delivery: { consumer: true, epoch: "probe" },
    }),
  });
  expect(answer.status).toBe(200);
  const delivery = DeliveryAnswerSchema.parse(await answer.json()).delivery;
  await fetch(`${envoyUrl}/v1/sessions/${probe}`, { method: "DELETE" });
  return delivery;
}

/** The names of the stream's consumers that are session consumers (`bus.SessionConsumerName`). */
async function sessionConsumerNames(observer: NatsConnection): Promise<string[]> {
  const manager = await observer.jetstreamManager();
  const names: string[] = [];
  for await (const consumer of manager.consumers.list("ENVOY_NOTIFICATIONS")) {
    if (consumer.name.startsWith("s-")) names.push(consumer.name);
  }
  return names;
}

/**
 * The freeze every session plugin must survive, in the delivery mode `host.stack`'s listener
 * grant decides.
 *
 * On consumer delivery (`listenerGrant: "current"`: the listener holds the session's durable
 * consumer and relays it on `notifications.envoy.deliver.<id>`) the session's registration is
 * answered `consumer` and its relay subject is subscribed, and a direct message is shown live.
 * Then the session's NATS path is frozen and a second message sent while it is cut off: it must be
 * shown within 60 s of the freeze, after the connection nats.js dialled in place of the frozen one
 * subscribed the relay subject again and the session registered a new epoch, so the listener
 * restarted the relay at the consumer's ack floor. A third message, sent after that registration,
 * is relayed on the new connection. Each of the three is shown once, and none as a replay: the
 * relay, not `POST /v1/replay`, brought the one sent during the freeze.
 *
 * On the core path (`listenerGrant: "before-session-consumers"`, the path every hub runs until its
 * NATS user carries session consumers) a registration asking for a consumer is answered
 * `core`/`no_grant`, no session consumer exists on the stream, and the session takes its messages
 * on its own core subscription, `notifications.agent.<id>`. The message sent while the path was
 * frozen reached nobody live: the session must be shown it within 60 s of the freeze all the same,
 * exactly once and as a replay, which is `POST /v1/replay`'s catch-up after the reconnect.
 *
 * The host runs on nats.js's real ping and dial timers against a real listener, so each wait is
 * real time.
 */
export async function runFreezeScenario(host: FreezeScenarioHost): Promise<void> {
  const { label, stack, sessionID, proxy, shown, waitFor } = host;
  const onCorePath = stack.listenerGrant === "before-session-consumers";
  // The subject the session's own connection takes the message on: its relay subject on consumer
  // delivery, its agent subject on the core path.
  const sessionSubject = onCorePath ? agentSubject(sessionID) : deliverSubject(sessionID);
  const send = async (marker: string) => {
    const answer = await fetch(`${stack.envoyUrl}/v1/messages/send`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ target_session: sessionID, source: "agent", message: marker }),
    });
    expect(answer.status).toBe(200);
  };
  const run = crypto.randomUUID();
  // The scenario's own connection, straight to natsd, reads the listener's session entry.
  const observer = await connect({ servers: [stack.natsUrl], name: `stall-observer-${run}` });
  try {
    const sessions = await observer.jetstream().views.kv("envoy_sessions", { bindOnly: true });
    const started = Date.now();
    let firstEpoch: string | undefined;
    await waitFor(
      async () => {
        const entry = await sessionEntry(sessions, sessionID);
        if (entry === undefined) return false;
        firstEpoch = entry.delivery === "consumer" ? entry.epoch : undefined;
        if (onCorePath ? entry.delivery !== undefined : firstEpoch === undefined) return false;
        return await subscribedSince(stack.monitorUrl, sessionSubject, 0);
      },
      started + 30_000,
      onCorePath
        ? "the session's registration recorded on the core path, with its agent subject subscribed"
        : "the session's registration answered consumer, with its relay subject subscribed"
    );
    if (onCorePath) {
      // The grant carries no session consumer, so a registration that asks for one is answered
      // core, and the stream holds no session consumer at all.
      expect(await registrationDelivery(stack.envoyUrl)).toEqual({
        mode: "core",
        reason: "no_grant",
      });
      expect(await sessionConsumerNames(observer)).toEqual([]);
    }

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
    // The connection that replaced the frozen one carries the session's subject.
    expect(await subscribedSince(stack.monitorUrl, sessionSubject, frozenAt)).toBe(true);
    const renewed = await sessionEntry(sessions, sessionID);
    if (onCorePath) {
      // The reconnect's registration left the session on the core path: no consumer, no epoch.
      expect(renewed?.delivery).toBeUndefined();
      expect(await sessionConsumerNames(observer)).toEqual([]);
    } else {
      // The relay restarted only on a registration that named a new epoch, still on consumer
      // delivery.
      expect(renewed?.delivery).toBe("consumer");
      expect(renewed?.epoch).toBeDefined();
      expect(renewed?.epoch).not.toBe(firstEpoch);
    }

    const after = `stall-live-after-${run}`;
    await send(after);
    await waitFor(
      () => shown(after).length > 0,
      Date.now() + 10_000,
      "the message after the reconnect"
    );

    // Each shown once. On consumer delivery the relay delivered all three, so none is a replay; on
    // the core path the two live ones came over the session's subscription and the frozen window's
    // over /v1/replay, which marks it.
    for (const marker of [live, missed, after]) {
      const messages = shown(marker);
      expect(messages).toHaveLength(1);
      expect(messages[0]?.includes(REPLAYED_SUFFIX)).toBe(onCorePath && marker === missed);
    }
  } finally {
    await observer.close();
  }
}

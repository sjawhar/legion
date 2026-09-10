import {
  AckPolicy,
  type ConnectionOptions,
  type ConsumerConfig,
  type ConsumerInfo,
  connect,
  consumerOpts,
  DeliverPolicy,
  ErrorCode,
  type NatsConnection,
  NatsError,
  nanos,
  StringCodec,
} from "nats";
import { createCancellableSleep } from "./cancellable-sleep";
import type { DaemonConfig } from "./config";

/**
 * The delivery outcomes a durable JetStream message can resolve to. Exactly
 * one of `ack`/`nak`/`term` is called per delivery, or the process exits
 * without calling any of them (see events.ts's `fatal` hook): `ack` after
 * a reducer's effects have all dispatched and its mutation is durably
 * saved; `nak(delayMs)` for a failure with no reducer or saved state
 * behind it (a GitHub mention's publish, including a 404 no holder), so
 * JetStream redelivers after the delay; `term(reason)` for a message that
 * can never succeed (malformed JSON, a schema/contract violation, or a
 * deterministic reducer throw) so it is never redelivered.
 */
export interface DurableMessageControl {
  /** The message's position in the stream, for poison/error log lines. */
  streamSequence: number;
  /** The message's position in this consumer's delivery order, for poison/error log lines. */
  deliverySequence: number;
  ack(): void;
  nak(delayMs?: number): void;
  term(reason?: string): void;
}

export interface NatsTransport {
  subscribe(subject: string, callback: (subject: string, data: string) => void): () => void;
  /**
   * Consumes a JetStream stream through a durable, server-owned consumer: the
   * consumer is created (or drift-corrected) via the JetStream manager and
   * then bound, never created by a plain `js.subscribe`, so the client
   * unsubscribing or reconnecting never deletes it (see the mirrored Go
   * listener at packages/envoy/cmd/listener/main.go:100-111). The callback
   * must resolve every delivery through exactly one of `control`'s
   * `ack`/`nak`/`term` methods; the consumer has unlimited redelivery
   * (`max_deliver: -1`), so an unresolved or nak'd message keeps retrying
   * until explicitly acked or termed.
   */
  consumeDurable(
    stream: string,
    durable: string,
    filterSubjects: string[],
    callback: (subject: string, data: string, control: DurableMessageControl) => void
  ): () => void;
  publish(subject: string, data: string): void;
  request(subject: string, data: string): Promise<string>;
  ready(): Promise<void>;
  /**
   * Flushes every frame already written to the client's outgoing buffer
   * (ack/nak/term calls included) out to the server, round-tripping a
   * PING/PONG so the caller knows the server has seen them. Used before a
   * fatal exit so a just-sent `term` is durably applied to the poison
   * message before the process disappears (see events.ts's
   * `processDurableMessage`) — otherwise JetStream can redeliver a message
   * the daemon already decided to never retry.
   */
  flush(): Promise<void>;
  close(): Promise<void>;
}

// Canonical durable-consumer policy, mirroring the Go listener's
// applyListenerConsumerPolicy (packages/envoy/cmd/listener/main.go:70-88): a
// consumer is created (or drift-corrected) via the JetStream manager and then
// bound, never created by a plain `js.pullSubscribe` — that path deletes the
// consumer it created on unsubscribe/drain (main.go:100-111), which would
// silently reset the durable cursor on every daemon restart or reconnect.
const DURABLE_CONSUMER_ACK_WAIT_MS = 60_000;
const DURABLE_CONSUMER_MAX_ACK_PENDING = 256;
// Unlimited: a message that can never succeed is termed explicitly (never
// left to exhaust a delivery cap and go silently undelivered); anything
// else nak's and keeps retrying indefinitely, since JetStream redelivery
// is the daemon's only durability mechanism for an event whose state
// mutation isn't confirmed saved yet (see events.ts).
const DURABLE_CONSUMER_MAX_DELIVER = -1;
const DURABLE_CONSUMER_INACTIVE_THRESHOLD_MS = 7 * 24 * 60 * 60 * 1000;
const DURABLE_CONSUMER_PULL_BATCH = 32;
const DURABLE_CONSUMER_PULL_EXPIRES_MS = 5_000;

function durableConsumerPolicy(filterSubjects: string[]): Partial<ConsumerConfig> {
  return {
    filter_subjects: filterSubjects,
    ack_policy: AckPolicy.Explicit,
    ack_wait: nanos(DURABLE_CONSUMER_ACK_WAIT_MS),
    max_ack_pending: DURABLE_CONSUMER_MAX_ACK_PENDING,
    max_deliver: DURABLE_CONSUMER_MAX_DELIVER,
    inactive_threshold: nanos(DURABLE_CONSUMER_INACTIVE_THRESHOLD_MS),
    deliver_policy: DeliverPolicy.All,
  };
}

export function durableConsumerPolicyDrifted(
  config: ConsumerConfig,
  filterSubjects: string[]
): boolean {
  const policy = durableConsumerPolicy(filterSubjects);
  const existingFilterSubjects = config.filter_subjects ?? [];
  return (
    config.filter_subject !== undefined ||
    config.deliver_subject !== undefined ||
    config.deliver_group !== undefined ||
    existingFilterSubjects.length !== filterSubjects.length ||
    existingFilterSubjects.some((value, index) => value !== filterSubjects[index]) ||
    config.ack_policy !== policy.ack_policy ||
    config.ack_wait !== policy.ack_wait ||
    config.max_ack_pending !== policy.max_ack_pending ||
    config.max_deliver !== policy.max_deliver ||
    config.inactive_threshold !== policy.inactive_threshold ||
    config.deliver_policy !== policy.deliver_policy
  );
}

/**
 * Builds the full config `jsm.consumers.update` should send for a
 * drift-corrected consumer. The client library's `update()` merges its
 * payload onto a freshly-fetched server config (`Object.assign(current,
 * payload)`), so any field the canonical policy doesn't explicitly set
 * survives from `current` untouched — including a legacy `filter_subject`
 * (mutually exclusive with `filter_subjects`) or a push-only
 * `deliver_subject`/`deliver_group` left over from a differently-configured
 * consumer that reused this durable name. Explicitly clearing those three
 * fields (as `undefined`, which JSON-encodes as absent) ensures the merged
 * config the server receives is actually valid for a pull consumer.
 */
export function durableConsumerUpdatePayload(
  current: ConsumerConfig,
  filterSubjects: string[]
): Partial<ConsumerConfig> {
  return {
    ...current,
    ...durableConsumerPolicy(filterSubjects),
    filter_subject: undefined,
    deliver_subject: undefined,
    deliver_group: undefined,
  };
}

const DURABLE_CONSUMER_RESTART_MIN_DELAY_MS = 5_000;
const DURABLE_CONSUMER_RESTART_MAX_DELAY_MS = 60_000;

/**
 * Runs `operation` repeatedly until `isCancelled()` reports true. A pull
 * subscription's async iterator can end on its own — the server deletes the
 * consumer after `inactive_threshold` during a long outage, the connection
 * resets, etc. — without anyone ever calling the returned unsubscribe
 * function; without a restart loop the daemon would silently stop consuming.
 * Each ended or failed attempt is reported once via `onRestart` and followed
 * by an exponential backoff (5s doubling to a 60s ceiling) before retrying.
 */
export async function runWithRestart(
  operation: () => Promise<void>,
  isCancelled: () => boolean,
  sleep: (delayMs: number) => Promise<void>,
  onRestart: (reason: unknown) => void
): Promise<void> {
  let delay = DURABLE_CONSUMER_RESTART_MIN_DELAY_MS;
  while (!isCancelled()) {
    try {
      await operation();
      if (isCancelled()) return;
      onRestart(new Error("durable consumer pull ended"));
    } catch (error) {
      if (isCancelled()) return;
      onRestart(error);
    }
    await sleep(delay);
    delay = Math.min(delay * 2, DURABLE_CONSUMER_RESTART_MAX_DELAY_MS);
  }
}

/** The minimal JetStream message shape `consumeDurable` reads from a delivered pull message. */
interface DurableJsMsg {
  subject: string;
  info: { streamSequence: number; deliverySequence: number };
  string(): string;
  ack(): void;
  nak(delayMs?: number): void;
  term(reason?: string): void;
}

/**
 * The minimal pull-subscription surface `consumeDurable` needs: an
 * async-iterable message source plus `pull`/`unsubscribe`. Narrowed (rather
 * than the real `JetStreamPullSubscription`) so a test can inject a fake
 * without implementing that interface's full surface.
 */
export interface MinimalPullSubscription {
  pull(opts: { batch: number; expires: number }): void;
  unsubscribe(): void;
  [Symbol.asyncIterator](): AsyncIterator<DurableJsMsg>;
}

/**
 * The minimal core-NATS subscription surface `subscribe()` needs. Narrowed
 * (rather than the real `Subscription`) so a test can inject a fake
 * connection without implementing that interface's full surface.
 */
export interface MinimalCoreSubscription {
  unsubscribe(): void;
  [Symbol.asyncIterator](): AsyncIterator<{ subject: string; data: Uint8Array }>;
}

/**
 * The subset of `NatsConnection` `createNatsTransport` actually uses,
 * narrowed so a test can inject a fake connection without implementing the
 * full client surface.
 */
export interface JetStreamConnection {
  jetstreamManager(): Promise<{
    consumers: {
      info(stream: string, durable: string): Promise<ConsumerInfo>;
      add(stream: string, config: Partial<ConsumerConfig>): Promise<ConsumerInfo>;
      update(
        stream: string,
        durable: string,
        config: Partial<ConsumerConfig>
      ): Promise<ConsumerInfo>;
    };
  }>;
  jetstream(): {
    pullSubscribe(subject: string, opts: unknown): Promise<MinimalPullSubscription>;
  };
  subscribe(subject: string): MinimalCoreSubscription;
  publish: NatsConnection["publish"];
  request(
    subject: string,
    data: Uint8Array,
    opts: { timeout: number }
  ): Promise<{ data: Uint8Array }>;
  flush(): Promise<void>;
  drain(): Promise<void>;
}

export async function createNatsTransport(
  config: DaemonConfig,
  connectFn: (opts: ConnectionOptions) => Promise<JetStreamConnection> = connect
): Promise<NatsTransport> {
  const connection = await connectFn({
    servers: config.natsUrls,
    name: `legion-daemon-${config.project}`,
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2_000,
  });
  const codec = StringCodec();
  const subscriptions = new Set<MinimalCoreSubscription>();
  const durableStops = new Set<() => void>();
  const durableRuns = new Set<Promise<void>>();

  return {
    subscribe(subject, callback) {
      const subscription = connection.subscribe(subject);
      subscriptions.add(subscription);
      void (async () => {
        for await (const message of subscription) {
          callback(message.subject, codec.decode(message.data));
        }
      })();
      return () => {
        subscriptions.delete(subscription);
        subscription.unsubscribe();
      };
    },
    consumeDurable(stream, durable, filterSubjects, callback) {
      let cancelled = false;
      let activeSubscription: MinimalPullSubscription | undefined;

      const ensureAndConsume = async (): Promise<void> => {
        const jsm = await connection.jetstreamManager();
        const policy = durableConsumerPolicy(filterSubjects);
        let info: ConsumerInfo | undefined;
        try {
          info = await jsm.consumers.info(stream, durable);
        } catch (error) {
          if (!(error instanceof NatsError) || error.code !== ErrorCode.JetStream404NoMessages) {
            throw error;
          }
        }
        if (!info) {
          await jsm.consumers.add(stream, { ...policy, durable_name: durable });
        } else if (durableConsumerPolicyDrifted(info.config, filterSubjects)) {
          await jsm.consumers.update(
            stream,
            durable,
            durableConsumerUpdatePayload(info.config, filterSubjects)
          );
        }
        if (cancelled) return;

        const subscription = await connection
          .jetstream()
          .pullSubscribe("", consumerOpts().bind(stream, durable).manualAck());
        if (cancelled) {
          subscription.unsubscribe();
          return;
        }
        activeSubscription = subscription;
        const pull = (): void => {
          subscription.pull({
            batch: DURABLE_CONSUMER_PULL_BATCH,
            expires: DURABLE_CONSUMER_PULL_EXPIRES_MS,
          });
        };
        pull();
        const pullTimer = setInterval(pull, DURABLE_CONSUMER_PULL_EXPIRES_MS);
        try {
          for await (const message of subscription) {
            callback(message.subject, message.string(), {
              streamSequence: message.info.streamSequence,
              deliverySequence: message.info.deliverySequence,
              ack: () => message.ack(),
              nak: (delayMs) => message.nak(delayMs),
              term: (reason) => message.term(reason),
            });
          }
        } finally {
          clearInterval(pullTimer);
          activeSubscription = undefined;
        }
      };

      const cancellableSleep = createCancellableSleep();
      const run = runWithRestart(
        ensureAndConsume,
        () => cancelled,
        cancellableSleep.sleep,
        (reason) => {
          console.error(
            `[legion] durable consumer ${durable} on ${stream} stopped unexpectedly; restarting: ${reason}`
          );
        }
      ).catch(() => {});
      durableRuns.add(run);

      const stop = (): void => {
        cancelled = true;
        activeSubscription?.unsubscribe();
        cancellableSleep.cancel();
      };
      durableStops.add(stop);

      return () => {
        durableStops.delete(stop);
        stop();
      };
    },
    publish(subject, data) {
      connection.publish(subject, codec.encode(data));
    },
    async request(subject, data) {
      const reply = await connection.request(subject, codec.encode(data), {
        timeout: 10_000,
      });
      return codec.decode(reply.data);
    },
    ready() {
      return connection.flush();
    },
    flush() {
      return connection.flush();
    },
    async close() {
      for (const subscription of subscriptions) subscription.unsubscribe();
      subscriptions.clear();
      for (const stop of durableStops) stop();
      durableStops.clear();
      await Promise.allSettled(durableRuns);
      durableRuns.clear();
      await connection.drain();
    },
  };
}

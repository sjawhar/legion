import {
  AckPolicy,
  type ConsumerConfig,
  type ConsumerInfo,
  connect,
  consumerOpts,
  DeliverPolicy,
  ErrorCode,
  type JetStreamPullSubscription,
  NatsError,
  nanos,
  StringCodec,
  type Subscription,
} from "nats";
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

export interface CancellableSleep {
  sleep(delayMs: number): Promise<void>;
  /** Resolves any in-flight `sleep` immediately and clears its timer; a no-op if none is pending. */
  cancel(): void;
}

/**
 * A `setTimeout`-backed sleep whose pending wait can be cut short. Without
 * this, `close()` returning before a `runWithRestart` backoff timer fires
 * would leave that timer (up to `DURABLE_CONSUMER_RESTART_MAX_DELAY_MS`)
 * alive and keeping the process from exiting cleanly.
 */
export function createCancellableSleep(): CancellableSleep {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let resolvePending: (() => void) | undefined;
  return {
    sleep(delayMs: number): Promise<void> {
      const { promise, resolve } = Promise.withResolvers<void>();
      resolvePending = resolve;
      timer = setTimeout(() => {
        timer = undefined;
        resolvePending = undefined;
        resolve();
      }, delayMs);
      return promise;
    },
    cancel(): void {
      if (timer === undefined) return;
      clearTimeout(timer);
      timer = undefined;
      resolvePending?.();
      resolvePending = undefined;
    },
  };
}

export async function createNatsTransport(config: DaemonConfig): Promise<NatsTransport> {
  const connection = await connect({
    servers: config.natsUrls,
    name: `legion-daemon-${config.project}`,
    reconnect: true,
    maxReconnectAttempts: -1,
    reconnectTimeWait: 2_000,
  });
  const codec = StringCodec();
  const subscriptions = new Set<Subscription>();
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
      let activeSubscription: JetStreamPullSubscription | undefined;

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

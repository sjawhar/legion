import { describe, expect, it } from "bun:test";
import {
  AckPolicy,
  type ConsumerConfig,
  type ConsumerInfo,
  DeliverPolicy,
  ErrorCode,
  NatsError,
  nanos,
  ReplayPolicy,
} from "nats";
import {
  createCancellableSleep,
  createNatsTransport,
  durableConsumerPolicyDrifted,
  durableConsumerUpdatePayload,
  runWithRestart,
} from "../nats-transport";
import { config } from "./ci-fixtures";

function baseConsumerConfig(overrides: Partial<ConsumerConfig> = {}): ConsumerConfig {
  return {
    durable_name: "legion-omp-github",
    ack_policy: AckPolicy.Explicit,
    deliver_policy: DeliverPolicy.All,
    replay_policy: ReplayPolicy.Instant,
    ...overrides,
  };
}

/** A durable delivery's ack/nak/term calls, recorded for assertions. */
interface FakeDurableCalls {
  acks: number;
  naks: Array<number | undefined>;
  terms: Array<string | undefined>;
}

class FakeJsMsg {
  constructor(
    readonly subject: string,
    private readonly data: string,
    readonly info: { streamSequence: number; deliverySequence: number },
    private readonly calls: FakeDurableCalls
  ) {}
  string(): string {
    return this.data;
  }
  ack(): void {
    this.calls.acks += 1;
  }
  nak(delayMs?: number): void {
    this.calls.naks.push(delayMs);
  }
  term(reason?: string): void {
    this.calls.terms.push(reason);
  }
}

/** A fake JetStream pull subscription: an async-iterable message queue the test pushes into, plus recorded `pull`/`unsubscribe` calls. */
class FakePullSubscription {
  readonly pulls: Array<{ batch: number; expires: number }> = [];
  unsubscribeCalls = 0;
  private readonly queue: FakeJsMsg[] = [];
  private ended = false;
  private wake?: () => void;

  push(message: FakeJsMsg): void {
    this.queue.push(message);
    this.wake?.();
  }
  end(): void {
    this.ended = true;
    this.wake?.();
  }
  pull(opts: { batch: number; expires: number }): void {
    this.pulls.push(opts);
  }
  unsubscribe(): void {
    this.unsubscribeCalls += 1;
    this.end();
  }
  async *[Symbol.asyncIterator](): AsyncIterator<FakeJsMsg> {
    for (;;) {
      const next = this.queue.shift();
      if (next) {
        yield next;
        continue;
      }
      if (this.ended) return;
      await new Promise<void>((resolve) => {
        this.wake = resolve;
      });
    }
  }
}

function fakeConsumerInfo(overrides: Partial<ConsumerConfig> = {}): ConsumerInfo {
  return { config: baseConsumerConfig(overrides) } as ConsumerInfo;
}

/** A fake JetStream manager: records every `consumers.info`/`add`/`update` call, and lets each test script the `info` outcome (missing, a specific config, or an arbitrary error). */
class FakeJsm {
  readonly infoCalls: Array<{ stream: string; durable: string }> = [];
  readonly addCalls: Array<{ stream: string; config: unknown }> = [];
  readonly updateCalls: Array<{ stream: string; durable: string; payload: unknown }> = [];
  infoBehavior: "missing" | { info: ConsumerInfo } | { error: Error } = "missing";

  consumers = {
    info: async (stream: string, durable: string): Promise<ConsumerInfo> => {
      this.infoCalls.push({ stream, durable });
      if (this.infoBehavior === "missing") {
        throw new NatsError("consumer not found", ErrorCode.JetStream404NoMessages);
      }
      if ("error" in this.infoBehavior) throw this.infoBehavior.error;
      return this.infoBehavior.info;
    },
    add: async (stream: string, consumerConfig: unknown): Promise<ConsumerInfo> => {
      this.addCalls.push({ stream, config: consumerConfig });
      return { config: consumerConfig } as ConsumerInfo;
    },
    update: async (stream: string, durable: string, payload: unknown): Promise<ConsumerInfo> => {
      this.updateCalls.push({ stream, durable, payload });
      return { config: payload } as ConsumerInfo;
    },
  };
}

interface FakeConnection {
  jetstreamManager(): Promise<FakeJsm>;
  jetstream(): { pullSubscribe(subject: string, opts: unknown): Promise<FakePullSubscription> };
  subscribe(): { unsubscribe(): void; [Symbol.asyncIterator](): AsyncIterator<never> };
  publish(): void;
  request(): Promise<{ data: Uint8Array }>;
  flush(): Promise<void>;
  drain(): Promise<void>;
}

function fakeConnection(jsm: FakeJsm, pullSubscription: FakePullSubscription): FakeConnection {
  return {
    jetstreamManager: async () => jsm,
    jetstream: () => ({ pullSubscribe: async () => pullSubscription }),
    subscribe: () => ({
      unsubscribe: () => {},
      [Symbol.asyncIterator]: async function* () {},
    }),
    publish: () => {},
    request: async () => ({ data: new Uint8Array() }),
    flush: async () => {},
    drain: async () => {},
  };
}

async function flushMicrotasks(): Promise<void> {
  for (let index = 0; index < 8; index += 1) await Promise.resolve();
}

describe("durableConsumerUpdatePayload", () => {
  it("clears a legacy filter_subject alongside push-only deliver_subject/deliver_group", () => {
    const current = baseConsumerConfig({
      filter_subject: "notifications.github.acme.widgets.>",
      deliver_subject: "_INBOX.legacy",
      deliver_group: "legacy-group",
    });

    const payload = durableConsumerUpdatePayload(current, ["notifications.github.acme.widgets.>"]);

    expect(payload.filter_subject).toBeUndefined();
    expect(payload.deliver_subject).toBeUndefined();
    expect(payload.deliver_group).toBeUndefined();
    expect(payload.filter_subjects).toEqual(["notifications.github.acme.widgets.>"]);
    expect(payload.ack_policy).toBe(AckPolicy.Explicit);
    expect(payload.deliver_policy).toBe(DeliverPolicy.All);
  });

  it("leaves a config with no legacy fields equivalent to the canonical policy", () => {
    const current = baseConsumerConfig({
      filter_subjects: ["notifications.github.acme.widgets.>"],
    });

    const payload = durableConsumerUpdatePayload(current, ["notifications.github.acme.widgets.>"]);

    expect(payload.filter_subject).toBeUndefined();
    expect(payload.filter_subjects).toEqual(["notifications.github.acme.widgets.>"]);
  });
});

function matchingConsumerConfig(filterSubjects: string[]): ConsumerConfig {
  return baseConsumerConfig({
    filter_subjects: filterSubjects,
    ack_wait: nanos(60_000),
    max_ack_pending: 256,
    max_deliver: -1,
    inactive_threshold: nanos(7 * 24 * 60 * 60 * 1000),
  });
}

describe("durableConsumerPolicyDrifted", () => {
  const subjects = ["notifications.github.acme.widgets.>"];

  it("reports no drift once every field already matches the canonical pull-consumer policy", () => {
    expect(durableConsumerPolicyDrifted(matchingConsumerConfig(subjects), subjects)).toBe(false);
  });

  it("treats a push consumer's leftover deliver_subject as drift, even with every other field matching", () => {
    const config = { ...matchingConsumerConfig(subjects), deliver_subject: "_INBOX.legacy" };

    expect(durableConsumerPolicyDrifted(config, subjects)).toBe(true);
  });

  it("treats a queue-group consumer's leftover deliver_group as drift", () => {
    const config = { ...matchingConsumerConfig(subjects), deliver_group: "legacy-group" };

    expect(durableConsumerPolicyDrifted(config, subjects)).toBe(true);
  });
});

describe("createCancellableSleep", () => {
  it("resolves an in-flight sleep immediately on cancel, without waiting out the delay", async () => {
    const cancellableSleep = createCancellableSleep();
    let settled = false;
    const promise = cancellableSleep.sleep(60_000).then(() => {
      settled = true;
    });

    expect(settled).toBe(false);
    cancellableSleep.cancel();
    await promise;

    expect(settled).toBe(true);
  });

  it("is a no-op when cancelled with no sleep pending", () => {
    const cancellableSleep = createCancellableSleep();

    expect(() => cancellableSleep.cancel()).not.toThrow();
  });
});

describe("runWithRestart", () => {
  it("restarts with backoff after the operation ends on its own, and stops once cancelled", async () => {
    let calls = 0;
    let cancelled = false;
    const sleeps: number[] = [];
    const restarts: unknown[] = [];
    await runWithRestart(
      async () => {
        calls += 1;
        if (calls === 2) cancelled = true;
      },
      () => cancelled,
      async (ms) => {
        sleeps.push(ms);
      },
      (reason) => {
        restarts.push(reason);
      }
    );
    expect(calls).toBe(2);
    expect(sleeps).toEqual([5_000]);
    expect(restarts).toHaveLength(1);
  });

  it("restarts with backoff after the operation throws", async () => {
    let calls = 0;
    let cancelled = false;
    const sleeps: number[] = [];
    const restarts: unknown[] = [];
    await runWithRestart(
      async () => {
        calls += 1;
        if (calls === 1) throw new Error("iterator failed");
        cancelled = true;
      },
      () => cancelled,
      async (ms) => {
        sleeps.push(ms);
      },
      (reason) => {
        restarts.push(reason);
      }
    );
    expect(calls).toBe(2);
    expect(sleeps).toEqual([5_000]);
    expect(restarts).toEqual([new Error("iterator failed")]);
  });

  it("never restarts when the first attempt is already cancelled", async () => {
    const sleeps: number[] = [];
    await runWithRestart(
      async () => {
        throw new Error("should not run");
      },
      () => true,
      async (ms) => {
        sleeps.push(ms);
      },
      () => {}
    );
    expect(sleeps).toEqual([]);
  });
});

describe("createNatsTransport consumeDurable", () => {
  it("creates the durable consumer via the JetStream manager when none exists", async () => {
    const jsm = new FakeJsm();
    const pullSub = new FakePullSubscription();
    const transport = await createNatsTransport(config(), async () => fakeConnection(jsm, pullSub));

    transport.consumeDurable(
      "ENVOY_NOTIFICATIONS",
      "legion-omp-github",
      ["notifications.github.acme.widgets.>"],
      () => {}
    );
    await flushMicrotasks();

    expect(jsm.infoCalls).toEqual([
      { stream: "ENVOY_NOTIFICATIONS", durable: "legion-omp-github" },
    ]);
    expect(jsm.addCalls).toHaveLength(1);
    expect(jsm.addCalls[0]?.stream).toBe("ENVOY_NOTIFICATIONS");
    expect((jsm.addCalls[0]?.config as ConsumerConfig | undefined)?.durable_name).toBe(
      "legion-omp-github"
    );
    expect(jsm.updateCalls).toHaveLength(0);

    await transport.close();
  });

  it("updates a drifted consumer via the JetStream manager instead of recreating it", async () => {
    const jsm = new FakeJsm();
    jsm.infoBehavior = { info: fakeConsumerInfo({ ack_wait: nanos(1_000) }) };
    const pullSub = new FakePullSubscription();
    const transport = await createNatsTransport(config(), async () => fakeConnection(jsm, pullSub));

    transport.consumeDurable(
      "ENVOY_NOTIFICATIONS",
      "legion-omp-github",
      ["notifications.github.acme.widgets.>"],
      () => {}
    );
    await flushMicrotasks();

    expect(jsm.addCalls).toHaveLength(0);
    expect(jsm.updateCalls).toHaveLength(1);
    expect(jsm.updateCalls[0]?.stream).toBe("ENVOY_NOTIFICATIONS");
    expect(jsm.updateCalls[0]?.durable).toBe("legion-omp-github");

    await transport.close();
  });

  it("skips create and update when the existing consumer already matches the canonical policy", async () => {
    const jsm = new FakeJsm();
    jsm.infoBehavior = {
      info: {
        config: matchingConsumerConfig(["notifications.github.acme.widgets.>"]),
      } as ConsumerInfo,
    };
    const pullSub = new FakePullSubscription();
    const transport = await createNatsTransport(config(), async () => fakeConnection(jsm, pullSub));

    transport.consumeDurable(
      "ENVOY_NOTIFICATIONS",
      "legion-omp-github",
      ["notifications.github.acme.widgets.>"],
      () => {}
    );
    await flushMicrotasks();

    expect(jsm.addCalls).toHaveLength(0);
    expect(jsm.updateCalls).toHaveLength(0);

    await transport.close();
  });

  it("propagates a non-404 consumers.info error instead of treating it as a missing consumer", async () => {
    const jsm = new FakeJsm();
    jsm.infoBehavior = { error: new Error("network failure") };
    const pullSub = new FakePullSubscription();
    const transport = await createNatsTransport(config(), async () => fakeConnection(jsm, pullSub));
    const restartReasons: string[] = [];
    const originalConsoleError = console.error;
    console.error = (...args: unknown[]) => {
      restartReasons.push(String(args[0]));
    };

    try {
      transport.consumeDurable(
        "ENVOY_NOTIFICATIONS",
        "legion-omp-github",
        ["notifications.github.acme.widgets.>"],
        () => {}
      );
      await flushMicrotasks();

      // The 404 short-circuit in `ensureAndConsume` only swallows the
      // specific "no consumer yet" code; any other consumers.info failure
      // must propagate to runWithRestart, not be treated as "create one".
      expect(jsm.addCalls).toHaveLength(0);
      expect(restartReasons.some((reason) => reason.includes("stopped unexpectedly"))).toBe(true);
      expect(restartReasons.some((reason) => reason.includes("network failure"))).toBe(true);
    } finally {
      console.error = originalConsoleError;
      await transport.close();
    }
  });

  it("delivers messages with control mapped to the underlying message's ack/nak/term and sequence", async () => {
    const jsm = new FakeJsm();
    const pullSub = new FakePullSubscription();
    const transport = await createNatsTransport(config(), async () => fakeConnection(jsm, pullSub));
    const calls: FakeDurableCalls = { acks: 0, naks: [], terms: [] };
    const delivered: Array<{ subject: string; data: string }> = [];
    const controls: Array<{ streamSequence: number; deliverySequence: number }> = [];

    transport.consumeDurable(
      "ENVOY_NOTIFICATIONS",
      "legion-omp-github",
      ["notifications.github.acme.widgets.>"],
      (subject, data, control) => {
        delivered.push({ subject, data });
        controls.push({
          streamSequence: control.streamSequence,
          deliverySequence: control.deliverySequence,
        });
        control.ack();
      }
    );
    await flushMicrotasks();
    pullSub.push(
      new FakeJsMsg(
        "notifications.github.acme.widgets.issue.1.comment",
        '{"hello":"world"}',
        { streamSequence: 7, deliverySequence: 2 },
        calls
      )
    );
    await flushMicrotasks();

    expect(delivered).toEqual([
      { subject: "notifications.github.acme.widgets.issue.1.comment", data: '{"hello":"world"}' },
    ]);
    expect(controls).toEqual([{ streamSequence: 7, deliverySequence: 2 }]);
    expect(calls).toEqual({ acks: 1, naks: [], terms: [] });

    await transport.close();
  });

  it("naks and terms through the same delivered control", async () => {
    const jsm = new FakeJsm();
    const pullSub = new FakePullSubscription();
    const transport = await createNatsTransport(config(), async () => fakeConnection(jsm, pullSub));
    const calls: FakeDurableCalls = { acks: 0, naks: [], terms: [] };
    let deliveries = 0;

    transport.consumeDurable(
      "ENVOY_NOTIFICATIONS",
      "legion-omp-github",
      ["notifications.github.acme.widgets.>"],
      (_subject, _data, control) => {
        deliveries += 1;
        if (deliveries === 1) control.nak(30_000);
        else control.term("poison");
      }
    );
    await flushMicrotasks();
    pullSub.push(
      new FakeJsMsg(
        "notifications.github.acme.widgets.issue.1.comment",
        "{}",
        { streamSequence: 1, deliverySequence: 1 },
        calls
      )
    );
    await flushMicrotasks();
    pullSub.push(
      new FakeJsMsg(
        "notifications.github.acme.widgets.issue.2.comment",
        "{}",
        { streamSequence: 2, deliverySequence: 1 },
        calls
      )
    );
    await flushMicrotasks();

    expect(calls).toEqual({ acks: 0, naks: [30_000], terms: ["poison"] });

    await transport.close();
  });

  it("pulls immediately on bind and unsubscribes the pull subscription when stopped", async () => {
    const jsm = new FakeJsm();
    const pullSub = new FakePullSubscription();
    const transport = await createNatsTransport(config(), async () => fakeConnection(jsm, pullSub));

    const unsubscribe = transport.consumeDurable(
      "ENVOY_NOTIFICATIONS",
      "legion-omp-github",
      ["notifications.github.acme.widgets.>"],
      () => {}
    );
    await flushMicrotasks();

    expect(pullSub.pulls.length).toBeGreaterThanOrEqual(1);

    unsubscribe();
    await flushMicrotasks();

    expect(pullSub.unsubscribeCalls).toBe(1);

    await transport.close();
  });
});

import { describe, expect, test } from "bun:test";
import {
  type ConsumerDeliveryOptions,
  type ConsumerDeliverySnapshot,
  type ConsumerFrame,
  type ConsumerGapNotice,
  createConsumerDelivery,
  DELIVER_SUBJECT_PREFIX,
  decodeRelayFrame,
  deliverSubject,
  FOLLOW_CLOCK_ALLOWANCE_MS,
  inboundUnavailable,
  RELAY_PROGRESS_INTERVAL_MS,
  type RelayMessage,
  type RelayReply,
  registerWithRelay,
} from "../consumer-delivery";
import { createDeliveryLedger, type DeliveryLedger, type LedgerSnapshot } from "../delivery-ledger";

const SESSION = "ses_a";
const AGENT = "notifications.agent.ses_a";
const ISSUE_TOPIC = "notifications.dispatch.issue.INFRA-1.>";
const ISSUE = "notifications.dispatch.issue.INFRA-1.comment";
const STREAM = "stream-1";
const T0 = Date.parse("2026-10-27T05:00:00Z");
const MINUTE = 60_000;

interface RelayInput {
  readonly seq: number;
  readonly subject?: string;
  readonly delivered?: number;
  readonly storedAt?: number;
  readonly gap?: boolean;
  readonly streamID?: string;
  readonly eventID?: string;
  readonly reply?: string;
}

function relay(input: RelayInput): RelayMessage {
  const headers: Record<string, string> = {
    "Envoy-Subject": input.subject ?? AGENT,
    "Envoy-Stream-Seq": String(input.seq),
    "Envoy-Stream-Id": input.streamID ?? STREAM,
    "Envoy-Delivered": String(input.delivered ?? 1),
    "Envoy-Stored-At": new Date(input.storedAt ?? T0 + MINUTE).toISOString(),
    ...(input.gap === true ? { "Envoy-Gap": "true" } : {}),
  };
  return {
    data: new TextEncoder().encode(
      JSON.stringify({ event_id: input.eventID ?? `evt-${input.seq}`, source: "agent" })
    ),
    headers: { get: (name) => headers[name] ?? "" },
    reply: input.reply ?? `_INBOX.${input.seq}.${input.delivered ?? 1}`,
  };
}

/** Intervals that fire as the fake clock moves. */
function fakeTimers(clock: { now: number }) {
  const running = new Set<{ readonly ms: number; next: number; readonly run: () => void }>();
  return {
    every(ms: number, run: () => void) {
      const timer = { ms, next: clock.now + ms, run };
      running.add(timer);
      return () => running.delete(timer);
    },
    advance(ms: number) {
      const until = clock.now + ms;
      for (;;) {
        const due = [...running]
          .filter((timer) => timer.next <= until)
          .sort((a, b) => a.next - b.next)[0];
        if (due === undefined) break;
        clock.now = due.next;
        due.next += due.ms;
        due.run();
      }
      clock.now = until;
    },
  };
}

function deliveryLedger(clock: { now: number }, written: { snapshot?: LedgerSnapshot } = {}) {
  return createDeliveryLedger({
    host: {
      sessionID: () => SESSION,
      followedSubjects: () => [AGENT],
      showFailureNotice: async () => undefined,
      failureNoticeFailed: () => undefined,
      log: () => undefined,
      write(change) {
        written.snapshot = change.snapshot();
      },
    },
    now: () => clock.now,
  });
}

function setup(
  options: {
    readonly clock?: { now: number };
    readonly ledger?: Pick<DeliveryLedger, "delivered">;
  } = {}
) {
  const clock = options.clock ?? { now: T0 };
  const timers = fakeTimers(clock);
  const replies: Array<readonly [string, RelayReply]> = [];
  const handed: ConsumerFrame[] = [];
  const running: Promise<void>[] = [];
  const gaps: ConsumerGapNotice[] = [];
  // The failure notices shown through the ledger (`liveFailed`).
  const failed: Array<{ readonly eventID: string | undefined }> = [];
  const logs: string[] = [];
  // The snapshots the host was asked to write.
  const persisted: ConsumerDeliverySnapshot[] = [];
  const state = {
    deliver: (_frame: ConsumerFrame): void | Promise<void> => undefined,
  };
  const config: ConsumerDeliveryOptions = {
    sessionID: SESSION,
    host: {
      deliver(frame) {
        handed.push(frame);
        const result = state.deliver(frame);
        if (result instanceof Promise) running.push(result);
        return result;
      },
      gap: (notice) => gaps.push(notice),
      log: (message) => logs.push(message),
    },
    publish: (reply, body) => replies.push([reply, body]),
    ledger: {
      delivered: (identity) => options.ledger?.delivered(identity) ?? false,
      liveFailed: async (envelope) => {
        failed.push({ eventID: envelope.event_id });
      },
    },
    persist: (snapshot) => {
      persisted.push(snapshot);
      return true;
    },
    now: () => clock.now,
    every: timers.every,
  };
  const delivery = createConsumerDelivery(config);
  /** Waits until every hand-off the host started has finished, those they let go included. */
  const settled = async () => {
    let seen: number;
    do {
      seen = running.length;
      await Promise.allSettled(running);
    } while (running.length !== seen);
  };
  return {
    clock,
    timers,
    replies,
    handed,
    gaps,
    failed,
    logs,
    state,
    delivery,
    settled,
    persisted,
  };
}

const seqs = (frames: readonly ConsumerFrame[]) => frames.map((frame) => frame.streamSeq);
const repliesTo = (replies: ReadonlyArray<readonly [string, RelayReply]>, reply: string) =>
  replies.filter(([to]) => to === reply).map(([, body]) => body);

describe("relay frames", () => {
  test("the relay subject is the session's deliver subject", () => {
    expect(deliverSubject(SESSION)).toBe("notifications.envoy.deliver.ses_a");
    expect(DELIVER_SUBJECT_PREFIX).toBe("notifications.envoy.deliver.");
    expect(setup().delivery.subject).toBe("notifications.envoy.deliver.ses_a");
  });

  test("decodes every header, a nanosecond stored time included", () => {
    const headers: Record<string, string> = {
      "Envoy-Subject": ISSUE,
      "Envoy-Stream-Seq": "42",
      "Envoy-Stream-Id": STREAM,
      "Envoy-Delivered": "3",
      "Envoy-Stored-At": "2026-10-27T05:00:01.123456789Z",
      "Envoy-Gap": "true",
    };
    const decoded = decodeRelayFrame({
      data: "{}",
      headers: { get: (name) => headers[name] ?? "" },
      reply: "_INBOX.x",
    });

    expect(decoded).toEqual({
      frame: {
        subject: ISSUE,
        streamSeq: 42,
        streamID: STREAM,
        delivered: 3,
        storedAt: "2026-10-27T05:00:01.123456789Z",
        storedAtMs: Date.parse("2026-10-27T05:00:01.123Z"),
        gap: true,
        data: "{}",
        reply: "_INBOX.x",
      },
    });
  });

  test("a frame that cannot decode is answered done and never handed over", () => {
    const { delivery, replies, handed, logs } = setup();
    delivery.receive({ data: "{}", headers: { get: () => "" }, reply: "_INBOX.bad" });

    expect(replies).toEqual([["_INBOX.bad", "done"]]);
    expect(handed).toEqual([]);
    expect(logs.some((line) => line.includes("undecodable"))).toBe(true);
  });
});

describe("replies", () => {
  test("received goes out before the host works on the frame", () => {
    const { delivery, replies, state } = setup();
    const seen: Array<ReadonlyArray<readonly [string, RelayReply]>> = [];
    state.deliver = () => {
      seen.push([...replies]);
    };

    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));

    expect(seen).toEqual([[["_INBOX.1", "received"]]]);
  });

  test("progress every 20 s while held, done after the write, and nothing after", () => {
    const { delivery, replies, handed, timers } = setup();
    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));

    timers.advance(RELAY_PROGRESS_INTERVAL_MS - 1);
    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received"]);
    timers.advance(1);
    timers.advance(RELAY_PROGRESS_INTERVAL_MS);
    handed[0]?.done();
    handed[0]?.done();
    handed[0]?.nak();
    timers.advance(5 * RELAY_PROGRESS_INTERVAL_MS);

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "progress", "progress", "done"]);
  });

  test("nak once, and no reply after it", () => {
    const { delivery, replies, handed, timers } = setup();
    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));

    handed[0]?.nak();
    handed[0]?.done();
    timers.advance(3 * RELAY_PROGRESS_INTERVAL_MS);

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "nak"]);
  });

  test("a frame the host throws on comes again, until its third delivery gives up", async () => {
    const { delivery, replies, failed, state, settled } = setup();
    state.deliver = () => Promise.reject(new Error("render failed"));

    delivery.receive(relay({ seq: 1, reply: "_INBOX.a" }));
    await settled();
    delivery.receive(relay({ seq: 1, delivered: 3, reply: "_INBOX.c" }));
    await settled();

    expect(repliesTo(replies, "_INBOX.a")).toEqual(["received", "nak"]);
    expect(repliesTo(replies, "_INBOX.c")).toEqual(["received", "done"]);
    expect(failed).toEqual([{ eventID: "evt-1" }]);
  });

  test("closing the session answers nak for every unsettled frame and any later one", () => {
    const { delivery, replies } = setup();
    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));
    delivery.receive(relay({ seq: 2, reply: "_INBOX.2" }));

    delivery.close();
    delivery.receive(relay({ seq: 3, reply: "_INBOX.3" }));

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "nak"]);
    expect(repliesTo(replies, "_INBOX.2")).toEqual(["received", "nak"]);
    expect(repliesTo(replies, "_INBOX.3")).toEqual(["nak"]);
  });
});

describe("hand-off order", () => {
  test("frames go to the host in stream order, one at a time", async () => {
    const { delivery, handed, replies, state, settled } = setup();
    const first = Promise.withResolvers<void>();
    state.deliver = (frame) => (frame.streamSeq === 1 ? first.promise : undefined);

    delivery.receive(relay({ seq: 1 }));
    delivery.receive(relay({ seq: 2, reply: "_INBOX.2" }));
    delivery.receive(relay({ seq: 3 }));

    expect(seqs(handed)).toEqual([1]);
    // The later frames' receipts did not wait for the first.
    expect(repliesTo(replies, "_INBOX.2")).toEqual(["received"]);

    first.resolve();
    await settled();

    expect(seqs(handed)).toEqual([1, 2, 3]);
  });

  test("a released side question does not hold the next frame", async () => {
    const { delivery, handed, replies, state, settled } = setup();
    const modelCall = Promise.withResolvers<void>();
    state.deliver = async (frame) => {
      if (frame.streamSeq !== 1) return;
      frame.release();
      await modelCall.promise;
      frame.done();
    };

    delivery.receive(relay({ seq: 1, reply: "_INBOX.1" }));
    delivery.receive(relay({ seq: 2, reply: "_INBOX.2" }));

    expect(seqs(handed)).toEqual([1, 2]);
    expect(repliesTo(replies, "_INBOX.2")).toEqual(["received"]);
    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received"]);

    modelCall.resolve();
    await settled();

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "done"]);
  });

  test("a redelivery of a held frame joins it: handed once, answered on both inboxes", () => {
    const { delivery, handed, replies } = setup();
    delivery.receive(relay({ seq: 4, reply: "_INBOX.first" }));

    delivery.receive(relay({ seq: 4, delivered: 2, reply: "_INBOX.again" }));
    handed[0]?.done();

    expect(seqs(handed)).toEqual([4]);
    expect(repliesTo(replies, "_INBOX.first")).toEqual(["received", "done"]);
    expect(repliesTo(replies, "_INBOX.again")).toEqual(["received", "done"]);
  });
});

describe("frames already written", () => {
  test("a frame the restored ledger holds is answered done and not handed over", async () => {
    const clock = { now: T0 };
    const persisted: { snapshot?: LedgerSnapshot } = {};
    const before = deliveryLedger(clock, persisted);
    const first = setup({ clock, ledger: before });
    first.state.deliver = async (frame) => {
      await before.recordDelivered({ event_id: `evt-${frame.streamSeq}` });
      frame.done();
    };
    first.delivery.receive(relay({ seq: 7, reply: "_INBOX.7" }));
    await first.settled();
    expect(repliesTo(first.replies, "_INBOX.7")).toEqual(["received", "done"]);

    // The process restarts; the relay restarts at the ack floor and sends the frame again.
    const after = deliveryLedger(clock);
    after.restore(persisted.snapshot ?? { ledger: [] });
    const second = setup({ clock, ledger: after });
    second.delivery.receive(relay({ seq: 7, delivered: 2, reply: "_INBOX.7b" }));
    second.delivery.receive(relay({ seq: 8, reply: "_INBOX.8" }));

    expect(repliesTo(second.replies, "_INBOX.7b")).toEqual(["received", "done"]);
    expect(seqs(second.handed)).toEqual([8]);
  });

  test("a copy written while it waited in the queue is answered done when its turn comes", async () => {
    const written = new Set<string>();
    const { delivery, handed, replies, state, settled } = setup({
      ledger: { delivered: (frame) => frame.event_id !== undefined && written.has(frame.event_id) },
    });
    const first = Promise.withResolvers<void>();
    state.deliver = (frame) => (frame.streamSeq === 1 ? first.promise : undefined);
    delivery.receive(relay({ seq: 1, eventID: "evt-same" }));
    delivery.receive(relay({ seq: 2, subject: AGENT, eventID: "evt-same", reply: "_INBOX.2" }));

    written.add("evt-same");
    handed[0]?.done();
    first.resolve();
    await settled();

    expect(seqs(handed)).toEqual([1]);
    expect(repliesTo(replies, "_INBOX.2")).toEqual(["received", "done"]);
  });
});

describe("follow filter", () => {
  test("a frame on a subject the session does not follow is answered done and dropped", () => {
    const { delivery, handed, replies } = setup();
    delivery.receive(relay({ seq: 1, subject: ISSUE, reply: "_INBOX.1" }));

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "done"]);
    expect(handed).toEqual([]);
  });

  test("a frame stored before its topic was followed is dropped; one stored after is shown", () => {
    const { delivery, handed, replies, clock } = setup();
    clock.now = T0 + 10 * MINUTE;
    delivery.setFollowed([ISSUE_TOPIC]);

    delivery.receive(relay({ seq: 1, subject: ISSUE, storedAt: T0, reply: "_INBOX.1" }));
    delivery.receive(
      relay({ seq: 2, subject: ISSUE, storedAt: T0 + 10 * MINUTE - FOLLOW_CLOCK_ALLOWANCE_MS })
    );
    delivery.receive(relay({ seq: 3, subject: ISSUE, storedAt: T0 + 11 * MINUTE }));
    // The base subject of a `.>` follow is followed too.
    delivery.receive(
      relay({ seq: 4, subject: "notifications.dispatch.issue.INFRA-1", storedAt: T0 + 11 * MINUTE })
    );

    expect(repliesTo(replies, "_INBOX.1")).toEqual(["received", "done"]);
    expect(seqs(handed)).toEqual([2, 3, 4]);
  });

  test("the agent subject is always taken, whenever it was stored", () => {
    const { delivery, handed } = setup();
    delivery.receive(relay({ seq: 1, subject: AGENT, storedAt: T0 - 60 * MINUTE }));

    expect(seqs(handed)).toEqual([1]);
  });

  test("a topic's follow time survives a restart, and an unfollow forgets it", () => {
    const { delivery, clock } = setup();
    delivery.setFollowed([ISSUE_TOPIC]);
    const snapshot = delivery.snapshot();

    clock.now = T0 + 30 * MINUTE;
    const restarted = setup({ clock });
    restarted.delivery.restore(snapshot);
    restarted.delivery.setFollowed([ISSUE_TOPIC]);
    restarted.delivery.receive(relay({ seq: 5, subject: ISSUE, storedAt: T0 + 5 * MINUTE }));
    expect(seqs(restarted.handed)).toEqual([5]);

    restarted.delivery.setFollowed([]);
    restarted.delivery.setFollowed([ISSUE_TOPIC]);
    restarted.delivery.receive(relay({ seq: 6, subject: ISSUE, storedAt: T0 + 6 * MINUTE }));
    expect(seqs(restarted.handed)).toEqual([5]);
  });
});

describe("late frames", () => {
  test("a redelivered frame, or one stored before the epoch began, is late", () => {
    const { delivery, handed, clock } = setup();
    delivery.receive(relay({ seq: 1, storedAt: T0 + MINUTE }));
    delivery.receive(relay({ seq: 2, storedAt: T0 + MINUTE, delivered: 2 }));
    delivery.receive(relay({ seq: 3, storedAt: T0 - MINUTE }));
    clock.now = T0 + 5 * MINUTE;
    delivery.beginEpoch();
    delivery.receive(relay({ seq: 4, storedAt: T0 + 2 * MINUTE }));
    delivery.receive(relay({ seq: 5, storedAt: T0 + 6 * MINUTE }));

    expect(handed.map((frame) => [frame.streamSeq, frame.late])).toEqual([
      [1, false],
      [2, true],
      [3, true],
      [4, true],
      [5, false],
    ]);
  });
});

describe("resume point", () => {
  test("below the lowest held frame, else the highest written; none before any", () => {
    const { delivery, handed } = setup();
    expect(delivery.resumePoint()).toBeUndefined();

    delivery.receive(relay({ seq: 5 }));
    delivery.receive(relay({ seq: 6 }));
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 4 });

    handed[1]?.done();
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 4 });
    handed[0]?.done();
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 6 });

    // A nak leaves the frame unwritten: the point stays below it.
    delivery.receive(relay({ seq: 9 }));
    handed[2]?.nak();
    expect(delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 8 });
  });

  test("a snapshot restores the point, and the registration carries it with the epoch", () => {
    const { delivery, handed, clock } = setup();
    delivery.receive(relay({ seq: 11 }));
    handed[0]?.done();
    const restarted = setup({ clock });

    restarted.delivery.restore(delivery.snapshot());

    expect(restarted.delivery.resumePoint()).toEqual({ stream_id: STREAM, seq: 11 });
    expect(restarted.delivery.registration()).toEqual({
      consumer: true,
      epoch: restarted.delivery.epoch().id,
      resume: { stream_id: STREAM, seq: 11 },
    });
    expect(setup().delivery.registration()).not.toHaveProperty("resume");
  });

  test("each epoch is a new random string", () => {
    const { delivery } = setup();
    const before = delivery.epoch().id;

    expect(delivery.beginEpoch().id).not.toBe(before);
    expect(delivery.registration().epoch).toBe(delivery.epoch().id);
  });
});

describe("gaps", () => {
  test("a gap the answer reports is told once, not again by the frame after it", () => {
    const { delivery, gaps } = setup();
    delivery.answered({
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: STREAM,
      gap: true,
    });
    delivery.receive(relay({ seq: 1, gap: true }));

    expect(gaps.map((notice) => notice.kind)).toEqual(["retention_gap"]);
  });

  test("a frame's gap is told when no answer reported it", () => {
    const { delivery, gaps } = setup();
    delivery.receive(relay({ seq: 1 }));
    delivery.receive(relay({ seq: 9, gap: true }));

    expect(gaps.map((notice) => notice.kind)).toEqual(["retention_gap"]);
  });

  test("an answer naming another stream than the resume point's is told once", () => {
    const { delivery, gaps } = setup();
    delivery.restore({ resume: { stream_id: "stream-old", seq: 90 }, follows: {} });

    const consumer = {
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: STREAM,
      gap: false,
    } as const;
    delivery.answered(consumer);
    delivery.answered(consumer);

    expect(gaps.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
    expect(delivery.resumePoint()).toBeUndefined();
  });

  // The listener starts the relay before it writes the registration's answer, and the relay's
  // frames come over NATS apart from that HTTP answer, so the first frame of the stream that
  // replaced the one the session resumed on can arrive before the answer naming it. The
  // replacement is still told, once.
  test("a frame from another stream than the resume point's, before the answer, is told once", () => {
    const { delivery, gaps } = setup();
    delivery.restore({ resume: { stream_id: "stream-old", seq: 9 }, follows: {} });

    delivery.registration();
    delivery.receive(relay({ seq: 1, streamID: "stream-new" }));
    delivery.answered({
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: "stream-new",
      gap: false,
    });
    delivery.receive(relay({ seq: 2, streamID: "stream-new" }));

    expect(gaps.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
  });

  test("a frame from the resume point's own stream, before the answer, tells nothing", () => {
    const { delivery, gaps } = setup();
    delivery.restore({ resume: { stream_id: "stream-old", seq: 9 }, follows: {} });

    delivery.registration();
    delivery.receive(relay({ seq: 10, streamID: "stream-old" }));
    delivery.answered({
      mode: "consumer",
      subject: deliverSubject(SESSION),
      stream_id: "stream-old",
      gap: false,
    });

    expect(gaps).toEqual([]);
  });
});

describe("what the registration resumes at", () => {
  test("nothing before the session has taken a frame: the consumer starts at the stream's end", () => {
    const { delivery } = setup();

    expect(delivery.registration()).not.toHaveProperty("resume");
  });

  test("the session's own resume point once it has taken a frame", () => {
    const { delivery, handed } = setup();
    delivery.receive(relay({ seq: 20 }));
    handed[0]?.done();

    expect(delivery.registration().resume).toEqual({ stream_id: STREAM, seq: 20 });
  });

  // A removal the stream kept while the session was away must not close a topic the session
  // followed again since (`closeReplayedRemoval`).
  test("a topic counts as followed before a frame only when the follow predates it", () => {
    const { delivery, clock } = setup();
    delivery.setFollowed([ISSUE_TOPIC]);
    clock.now = T0 + MINUTE;

    expect(delivery.followedBefore(ISSUE_TOPIC, T0 + MINUTE)).toBe(true);
    expect(delivery.followedBefore(ISSUE_TOPIC, T0 - FOLLOW_CLOCK_ALLOWANCE_MS)).toBe(true);
    expect(delivery.followedBefore(ISSUE_TOPIC, T0 - FOLLOW_CLOCK_ALLOWANCE_MS - 1)).toBe(false);
    expect(delivery.followedBefore("notifications.team.alpha", T0 + MINUTE)).toBe(false);
  });
});

describe("persisting the snapshot", () => {
  test("a change is written once; what a restore brought back is not written again", () => {
    const { delivery, handed, persisted, clock } = setup();
    delivery.setFollowed([ISSUE_TOPIC]);
    delivery.setFollowed([ISSUE_TOPIC]);
    delivery.receive(relay({ seq: 4 }));
    handed[0]?.done();
    delivery.persist();
    delivery.persist();

    expect(persisted).toEqual([
      { follows: { [ISSUE_TOPIC]: T0 } },
      { resume: { stream_id: STREAM, seq: 4 }, follows: { [ISSUE_TOPIC]: T0 } },
    ]);

    const restarted = setup({ clock });
    restarted.delivery.restore(delivery.snapshot());
    restarted.delivery.setFollowed([ISSUE_TOPIC]);
    restarted.delivery.persist();

    expect(restarted.persisted).toEqual([]);
  });

  test("the session's own agent subject is not one of the follow times", () => {
    const { delivery } = setup();
    delivery.setFollowed([AGENT, ISSUE_TOPIC]);

    expect(delivery.snapshot().follows).toEqual({ [ISSUE_TOPIC]: T0 });
  });
});

describe("one registration", () => {
  const UNCAPTURED = "notifications.team";
  const TEAM = "notifications.team.deploys";

  /** A relay that records what a registration stamped on it. */
  function fakeRelay() {
    const followed: string[][] = [];
    return {
      followed,
      setFollowed: (topics: Iterable<string>) => followed.push([...topics]),
      registration: () => ({ consumer: true, epoch: "epoch-1" }) as const,
    };
  }

  test("carries the topics the listener will store, and takes the answer", async () => {
    const relay = fakeRelay();
    const told: string[] = [];
    const answers: string[] = [];
    const sent: { topics: readonly string[]; epoch: string }[] = [];

    const result = await registerWithRelay({
      relay,
      ownAgentSubject: AGENT,
      // The bare base a stored row or a transcript brings back: the listener stores it for no
      // session and warns about it, so it never goes out (INFRA-771 review).
      followed: [TEAM, UNCAPTURED, "notifications.agent.ses_b"],
      outage: inboundUnavailable((why) => told.push(why)),
      send: (topics, delivery) => {
        sent.push({ topics, epoch: delivery.epoch });
        return Promise.resolve("registered");
      },
      answered: (answer) => answers.push(answer),
    });

    expect(sent).toEqual([{ topics: [AGENT, TEAM], epoch: "epoch-1" }]);
    expect(relay.followed).toEqual([[AGENT, TEAM]]);
    expect([result, answers, told]).toEqual(["registered", ["registered"], []]);
  });

  test("a session holding no relay subject registers nothing and is told once", async () => {
    const told: string[] = [];
    const outage = inboundUnavailable((why) => told.push(why));
    const sent: unknown[] = [];
    const register = () =>
      registerWithRelay({
        relay: undefined,
        ownAgentSubject: AGENT,
        followed: [TEAM],
        outage,
        send: (topics) => {
          sent.push(topics);
          return Promise.resolve("registered");
        },
        answered: () => undefined,
      });

    await expect(register()).rejects.toThrow("holds no relay subject");
    // The second outage tick says nothing more: the session was told once.
    await expect(register()).rejects.toThrow("holds no relay subject");
    expect(sent).toEqual([]);
    expect(told).toEqual([
      "this session holds no relay subject, so the listener can give it no consumer and it can receive nothing",
    ]);
  });

  test("a refused registration tells the session once and takes no answer", async () => {
    const relay = fakeRelay();
    const told: string[] = [];
    const answers: string[] = [];

    await expect(
      registerWithRelay({
        relay,
        ownAgentSubject: AGENT,
        followed: [TEAM],
        outage: inboundUnavailable((why) => told.push(why)),
        send: () => Promise.reject(new Error("no_grant")),
        answered: (answer: string) => answers.push(answer),
      })
    ).rejects.toThrow("no_grant");

    expect(relay.followed).toEqual([[AGENT, TEAM]]);
    expect([answers, told]).toEqual([[], ["no_grant"]]);
  });
});

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
  RELAY_PROGRESS_INTERVAL_MS,
  type RelayMessage,
  type RelayReply,
  registeredWithoutRelay,
} from "../consumer-delivery";
import {
  createReplayEngine,
  type ReplayEngine,
  type ReplaySnapshot,
  type ReplayState,
} from "../replay";

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

function replayEngine(clock: { now: number }, written: { snapshot?: ReplaySnapshot } = {}) {
  return createReplayEngine({
    host: {
      sessionID: () => SESSION,
      followedSubjects: () => [AGENT],
      deliver: async () => "shown",
      showFailureNotice: async () => undefined,
      failureNoticeFailed: () => undefined,
      notify: () => undefined,
      log: () => undefined,
      write(change) {
        written.snapshot = change.snapshot();
      },
    },
    transport: {
      replay: () => Promise.reject(new Error("no replay call is expected")),
    },
    now: () => clock.now,
  });
}

function setup(
  options: {
    readonly clock?: { now: number };
    readonly ledger?: Pick<ReplayEngine, "delivered">;
    /** The replay state the ledger reports: the cursors the core path left. */
    readonly state?: ReplayState;
  } = {}
) {
  const clock = options.clock ?? { now: T0 };
  const timers = fakeTimers(clock);
  const replies: Array<readonly [string, RelayReply]> = [];
  const handed: ConsumerFrame[] = [];
  const running: Promise<void>[] = [];
  const gaps: ConsumerGapNotice[] = [];
  // The failure notices shown through the ledger (`liveFailed`).
  const failed: Array<{
    readonly subject: string;
    readonly eventID: string | undefined;
    readonly receivedAt: number;
  }> = [];
  const logs: string[] = [];
  // The snapshots the host was asked to write, and every `setConsumerMode` the ledger was told.
  const persisted: ConsumerDeliverySnapshot[] = [];
  const consumerMode: boolean[] = [];
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
      liveFailed: async (followed, envelope, _error, receivedAt) => {
        failed.push({ subject: followed, eventID: envelope.event_id, receivedAt });
      },
      state: () => options.state ?? { subjects: {} },
      setConsumerMode: (on) => consumerMode.push(on),
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
    consumerMode,
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
    expect(failed).toEqual([{ subject: AGENT, eventID: "evt-1", receivedAt: T0 }]);
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
    const persisted: { snapshot?: ReplaySnapshot } = {};
    const before = replayEngine(clock, persisted);
    const first = setup({ clock, ledger: before });
    first.state.deliver = async (frame) => {
      await before.recordDelivered({ event_id: `evt-${frame.streamSeq}` });
      frame.done();
    };
    first.delivery.receive(relay({ seq: 7, reply: "_INBOX.7" }));
    await first.settled();
    expect(repliesTo(first.replies, "_INBOX.7")).toEqual(["received", "done"]);

    // The process restarts; the relay restarts at the ack floor and sends the frame again.
    const after = replayEngine(clock);
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
    const mode = delivery.answered(
      {
        mode: "consumer",
        subject: deliverSubject(SESSION),
        stream_id: STREAM,
        gap: true,
      },
      [AGENT]
    );
    delivery.receive(relay({ seq: 1, gap: true }));

    expect(mode).toBe("consumer");
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
    delivery.answered(consumer, [AGENT]);
    delivery.answered(consumer, [AGENT]);

    expect(gaps.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
    expect(delivery.resumePoint()).toBeUndefined();
  });

  test("a core answer tells nothing", () => {
    const { delivery, gaps } = setup();
    delivery.restore({ resume: { stream_id: "stream-old", seq: 90 }, follows: {} });

    expect(delivery.answered({ mode: "core", reason: "no_grant" }, [AGENT])).toBe("core");
    expect(gaps).toEqual([]);
  });

  // A session moving from the core path registers the resume its ledger's cursors seed
  // (`seededResume`), which names the stream those cursors were read on. When the listener
  // answers on another stream, that one was replaced: its consumer starts at the new stream's
  // end, and the messages the cursors still owed it are gone.
  test("an answer naming another stream than the seeded resume's is told once", () => {
    const { delivery, gaps } = setup({
      state: { stream_id: "stream-old", subjects: { [AGENT]: { since: 2, after: 9 } } },
    });
    const answer = (stream: string) =>
      ({
        mode: "consumer",
        subject: deliverSubject(SESSION),
        stream_id: stream,
        gap: false,
      }) as const;

    expect(delivery.registration().resume).toEqual({ stream_id: "stream-old", seq: 9 });
    delivery.answered(answer("stream-new"), [AGENT]);
    delivery.answered(answer("stream-new"), [AGENT]);

    expect(gaps.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
  });

  test("an answer naming the seeded resume's own stream tells nothing", () => {
    const { delivery, gaps } = setup({
      state: { stream_id: "stream-old", subjects: { [AGENT]: { since: 2, after: 9 } } },
    });

    delivery.registration();
    delivery.answered(
      { mode: "consumer", subject: deliverSubject(SESSION), stream_id: "stream-old", gap: false },
      [AGENT]
    );

    expect(gaps).toEqual([]);
  });

  // The listener starts the relay before it writes the registration's answer, and the relay's
  // frames come over NATS apart from that HTTP answer, so the first frame from the new stream can
  // arrive before the answer that names it. The replacement is still told, once.
  test("a frame from another stream than the seeded resume's, before the answer, is told once", () => {
    const { delivery, gaps } = setup({
      state: { stream_id: "stream-old", subjects: { [AGENT]: { since: 2, after: 9 } } },
    });

    delivery.registration();
    delivery.receive(relay({ seq: 1, streamID: "stream-new" }));
    delivery.answered(
      { mode: "consumer", subject: deliverSubject(SESSION), stream_id: "stream-new", gap: false },
      [AGENT]
    );
    delivery.receive(relay({ seq: 2, streamID: "stream-new" }));

    expect(gaps.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
  });

  test("a frame from the seeded resume's own stream, before the answer, tells nothing", () => {
    const { delivery, gaps } = setup({
      state: { stream_id: "stream-old", subjects: { [AGENT]: { since: 2, after: 9 } } },
    });

    delivery.registration();
    delivery.receive(relay({ seq: 10, streamID: "stream-old" }));
    delivery.answered(
      { mode: "consumer", subject: deliverSubject(SESSION), stream_id: "stream-old", gap: false },
      [AGENT]
    );

    expect(gaps).toEqual([]);
  });
});

describe("the move from the core path to consumer delivery", () => {
  const CONSUMER = {
    mode: "consumer",
    subject: deliverSubject(SESSION),
    stream_id: STREAM,
    gap: false,
  } as const;

  test("with no snapshot, the registration resumes at the lowest cursor the sweeps left", () => {
    const { delivery } = setup({
      state: {
        stream_id: STREAM,
        subjects: { [AGENT]: { since: 2, after: 12 }, [ISSUE_TOPIC]: { since: 3, after: 7 } },
      },
    });

    expect(delivery.registration().resume).toEqual({ stream_id: STREAM, seq: 7 });
  });

  test("a cursor of a topic the stream does not store is not one the relay resumes at", () => {
    const { delivery } = setup({
      state: {
        stream_id: STREAM,
        subjects: { [AGENT]: { after: 12 }, "notifications.team.alpha": { after: 1 } },
      },
    });

    expect(delivery.registration().resume).toEqual({ stream_id: STREAM, seq: 12 });
  });

  test("a subject no sweep has read yet, and a state without a stream, seed nothing", () => {
    const unread = setup({ state: { stream_id: STREAM, subjects: { [AGENT]: {} } } });
    const older = setup({
      state: { stream_created: "2026-10-01T00:00:00Z", subjects: { [AGENT]: { after: 4 } } },
    });

    expect(unread.delivery.registration()).not.toHaveProperty("resume");
    expect(older.delivery.registration()).not.toHaveProperty("resume");
  });

  test("the session's own resume point is the one registered once it has taken a frame", () => {
    const { delivery, handed } = setup({
      state: { stream_id: STREAM, subjects: { [AGENT]: { after: 7 } } },
    });
    delivery.receive(relay({ seq: 20 }));
    handed[0]?.done();

    expect(delivery.registration().resume).toEqual({ stream_id: STREAM, seq: 20 });
  });

  test("a message stored past the cursor is shown once, and the ledger is in consumer mode", () => {
    const { delivery, handed, consumerMode } = setup({
      state: { stream_id: STREAM, subjects: { [AGENT]: { after: 7 } } },
    });

    const registration = delivery.registration();
    expect(registration.resume).toEqual({ stream_id: STREAM, seq: 7 });
    delivery.answered(CONSUMER, [AGENT]);
    // The listener creates the consumer at the resume point plus one: message 8 is relayed.
    delivery.receive(relay({ seq: 8 }));
    delivery.receive(relay({ seq: 8, delivered: 2 }));

    expect(seqs(handed)).toEqual([8]);
    expect(consumerMode).toEqual([true]);
  });

  test("a consumer answer relays the stored topics it carried, and a core one none", () => {
    const { delivery, consumerMode } = setup();

    delivery.answered(CONSUMER, [AGENT, ISSUE_TOPIC, "notifications.team.alpha"]);
    expect(delivery.relaying()).toBe(true);
    expect(delivery.relays(AGENT)).toBe(true);
    expect(delivery.relays(ISSUE_TOPIC)).toBe(true);
    expect(delivery.relays("notifications.team.alpha")).toBe(false);

    delivery.answered({ mode: "core", reason: "not_answered" }, [AGENT, ISSUE_TOPIC]);
    expect(delivery.relaying()).toBe(false);
    expect(delivery.relays(ISSUE_TOPIC)).toBe(false);
    expect(consumerMode).toEqual([true, false]);
  });

  test("the relay closing leaves consumer mode and relays nothing", () => {
    const { delivery, consumerMode } = setup();
    delivery.answered(CONSUMER, [AGENT, ISSUE_TOPIC]);

    delivery.close();

    expect(delivery.relaying()).toBe(false);
    expect(consumerMode).toEqual([true, false]);
  });

  // A session that ran the released plugin comes back with the core path's cursors and no
  // consumer snapshot, so `setFollowed` stamps every topic it follows at the resume time and the
  // follow times say nothing about what it was owed. The cursors are what say since which
  // sequence it followed each topic, so the comments stored while it was away are shown.
  describe("a session resuming with cursors and no snapshot", () => {
    const ROLE_TOPIC = "notifications.role.reviewer";
    const UNFOLLOWED = "notifications.dispatch.issue.OTHER-1.comment";
    const RESUMED = T0 + 60 * MINUTE;
    const state: ReplayState = {
      stream_id: STREAM,
      subjects: {
        [AGENT]: { since: 2, after: 9 },
        [ISSUE_TOPIC]: { since: 3, after: 12 },
        [ROLE_TOPIC]: { since: 1, after: 1 },
      },
    };

    /** The session back an hour later, following again what its cursors hold. */
    const resumed = () => {
      const session = setup({ clock: { now: RESUMED }, state });
      session.delivery.setFollowed([AGENT, ISSUE_TOPIC, ROLE_TOPIC]);
      session.delivery.answered(CONSUMER, [AGENT, ISSUE_TOPIC, ROLE_TOPIC]);
      return session;
    };

    test("it seeds at the lowest stored cursor, and every topic is stamped at the resume", () => {
      const { delivery } = resumed();

      // The role topic's cursor is the lowest, but the stream stores no role lane, so the agent
      // subject's is the seed.
      expect(delivery.registration().resume).toEqual({ stream_id: STREAM, seq: 9 });
      expect(delivery.snapshot().follows).toEqual({
        [ISSUE_TOPIC]: RESUMED,
        [ROLE_TOPIC]: RESUMED,
      });
    });

    test("a comment stored while it was away, past its topic's cursor, is shown once", () => {
      const { delivery, handed, replies } = resumed();

      delivery.receive(relay({ seq: 20, subject: ISSUE, storedAt: T0 }));
      delivery.receive(relay({ seq: 20, subject: ISSUE, storedAt: T0, delivered: 2 }));
      handed[0]?.done();

      expect(seqs(handed)).toEqual([20]);
      expect(repliesTo(replies, "_INBOX.20.1")).toEqual(["received", "done"]);
    });

    test("a role forward stored on the agent subject while it was away is shown once", () => {
      const { delivery, handed } = resumed();

      delivery.receive(relay({ seq: 21, storedAt: T0, eventID: "evt-role-forward" }));
      delivery.receive(relay({ seq: 21, storedAt: T0, delivered: 2, eventID: "evt-role-forward" }));

      expect(seqs(handed)).toEqual([21]);
    });

    test("a comment stored at its topic's cursor, which the sweeps handled, stays dropped", () => {
      const { delivery, handed, replies } = resumed();

      delivery.receive(relay({ seq: 3, subject: ISSUE, storedAt: T0 }));

      expect(seqs(handed)).toEqual([]);
      expect(repliesTo(replies, "_INBOX.3.1")).toEqual(["received", "done"]);
    });

    test("a topic followed on neither path stays dropped", () => {
      const { delivery, handed, replies } = resumed();

      delivery.receive(relay({ seq: 30, subject: UNFOLLOWED, storedAt: T0 }));

      expect(seqs(handed)).toEqual([]);
      expect(repliesTo(replies, "_INBOX.30.1")).toEqual(["received", "done"]);
    });
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

describe("a registration that asked for no relay", () => {
  test("leaves the core path on through the delivery open for it", () => {
    const { delivery, consumerMode } = setup();
    const modes: boolean[] = [];
    delivery.answered(
      { mode: "consumer", subject: deliverSubject(SESSION), stream_id: STREAM, gap: false },
      [AGENT, ISSUE_TOPIC]
    );

    registeredWithoutRelay({ setConsumerMode: (on) => modes.push(on) }, delivery, [
      AGENT,
      ISSUE_TOPIC,
    ]);

    expect(delivery.relaying()).toBe(false);
    expect(delivery.relays(ISSUE_TOPIC)).toBe(false);
    // The delivery is the one that tells its own ledger; the caller's is not written past it.
    expect(consumerMode).toEqual([true, false]);
    expect(modes).toEqual([]);
  });

  test("with no delivery open, the ledger alone leaves consumer mode", () => {
    const modes: boolean[] = [];

    registeredWithoutRelay({ setConsumerMode: (on) => modes.push(on) }, undefined, [AGENT]);

    expect(modes).toEqual([false]);
  });
});

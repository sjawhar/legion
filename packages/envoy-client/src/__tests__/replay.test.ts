import { describe, expect, test } from "bun:test";
import type { Envelope } from "@legion/contracts";
import { createDeliveryDedupe, type DeliveryDedupe } from "../delivery";
import {
  createReplayEngine,
  REPLAY_LEDGER_LIMIT,
  REPLAY_LEDGER_TTL_MS,
  type ReplayChange,
  type ReplayDeliveryOutcome,
  type ReplayEngine,
  type ReplayFailureNotice,
  type ReplayFrame,
  type ReplayHost,
  type ReplayNotice,
  type ReplaySnapshot,
} from "../replay";
import { createEnvoyClient, type ReplayInput, type ReplayResult } from "../transport";

const SESSION = "ses_a";
const AGENT = "notifications.agent.ses_a";
const ISSUE = "notifications.dispatch.issue.INFRA-1.>";
const HOUR = 60 * 60 * 1000;
const T0 = Date.parse("2026-10-26T12:00:00Z");

interface Stored {
  readonly seq: number;
  readonly subject: string;
  readonly time: string;
  readonly data: string;
}

/** NATS subject matching: `*` is one token, `>` one or more trailing tokens. */
function subjectMatches(filter: string, subject: string): boolean {
  const wanted = filter.split(".");
  const tokens = subject.split(".");
  for (let index = 0; index < wanted.length; index += 1) {
    if (wanted[index] === ">") return tokens.length > index;
    if (index >= tokens.length) return false;
    if (wanted[index] !== "*" && wanted[index] !== tokens[index]) return false;
  }
  return wanted.length === tokens.length;
}

/**
 * The listener's replay over a model of the stream: the watermark read once, subjects read in
 * request order under one message budget, a subject without `after` started at the watermark.
 */
function createStream(clock: { now: number }) {
  let created = "2026-10-01T00:00:00Z";
  let messages: Stored[] = [];
  let lastSeq = 0;
  let firstSeq = 1;
  const refused = new Map<string, string>();
  const calls: ReplayInput[] = [];
  const failures = new Map<number, unknown>();
  let gate: Promise<void> | undefined;

  const answer = (input: ReplayInput): ReplayResult => {
    const watermark = lastSeq;
    let budget = input.maxMessages ?? 100;
    return {
      stream: { created, first_seq: firstSeq, last_seq: watermark },
      subjects: input.subjects.map(({ subject, after }) => {
        const reason = refused.get(subject);
        if (reason !== undefined) {
          return { subject, messages: [], next_after: after ?? 0, more: false, refused: reason };
        }
        if (after === undefined)
          return { subject, messages: [], next_after: watermark, more: false };
        const found: Stored[] = [];
        let cursor = after;
        while (cursor < watermark) {
          if (budget <= 0) return { subject, messages: found, next_after: cursor, more: true };
          const from = cursor;
          const next = messages.find(
            (message) =>
              message.seq > from &&
              message.seq <= watermark &&
              subjectMatches(subject, message.subject)
          );
          if (next === undefined) break;
          found.push(next);
          budget -= 1;
          cursor = next.seq;
        }
        return { subject, messages: found, next_after: watermark, more: false };
      }),
    };
  };

  return {
    calls,
    refused,
    /** The call with this 1-based number throws `error` instead of answering. */
    failCall(call: number, error: unknown) {
      failures.set(call, error);
    },
    /** Holds every call until the returned release runs. */
    hold(): () => void {
      const { promise, resolve } = Promise.withResolvers<void>();
      gate = promise;
      return () => {
        gate = undefined;
        resolve();
      };
    },
    publish(subject: string, envelope: Envelope | string): number {
      lastSeq += 1;
      messages.push({
        seq: lastSeq,
        subject,
        time: new Date(clock.now).toISOString(),
        data: typeof envelope === "string" ? envelope : JSON.stringify(envelope),
      });
      return lastSeq;
    },
    /** Retention dropped every message below `seq`. */
    expireBelow(seq: number) {
      messages = messages.filter((message) => message.seq >= seq);
      firstSeq = seq;
    },
    recreate(newCreated: string) {
      created = newCreated;
      messages = [];
      lastSeq = 0;
      firstSeq = 1;
    },
    answer,
    transport: {
      async replay(input: ReplayInput): Promise<ReplayResult> {
        calls.push(structuredClone(input));
        if (gate !== undefined) await gate;
        const failure = failures.get(calls.length);
        if (failure !== undefined) throw failure;
        return answer(input);
      },
    },
  };
}

interface Write {
  readonly delivered: ReplayChange["delivered"];
  readonly state: ReplayChange["state"];
  readonly snapshot: () => ReplaySnapshot;
}

function createHost(followed: string[]) {
  const host = {
    session: SESSION as string | undefined,
    followed,
    attempts: [] as ReplayFrame[],
    shown: [] as ReplayFrame[],
    notices: [] as ReplayNotice[],
    failureNotices: [] as ReplayFailureNotice[],
    noticeWarnings: [] as ReplayFailureNotice[],
    logs: [] as string[],
    writes: [] as Write[],
    deliverWith: (_frame: ReplayFrame): ReplayDeliveryOutcome | Promise<ReplayDeliveryOutcome> =>
      "shown",
    noticeWith: (_notice: ReplayFailureNotice): void => {},
  };
  const replayHost: ReplayHost = {
    sessionID: () => host.session,
    followedSubjects: () => host.followed,
    async deliver(frame) {
      host.attempts.push(frame);
      const outcome = await host.deliverWith(frame);
      if (outcome === "shown") host.shown.push(frame);
      return outcome;
    },
    async showFailureNotice(notice) {
      host.noticeWith(notice);
      host.failureNotices.push(notice);
    },
    failureNoticeFailed: (notice) => host.noticeWarnings.push(notice),
    notify: (notice) => host.notices.push(notice),
    log: (message) => host.logs.push(message),
    write(change) {
      host.writes.push({
        delivered: change.delivered,
        state: change.state,
        snapshot: () => change.snapshot(),
      });
    },
  };
  return { host, replayHost };
}

let counter = 0;
function envelope(overrides: Partial<Envelope> = {}): Envelope {
  counter += 1;
  return {
    event_id: `evt-${counter}`,
    source: "agent",
    source_event_id: `src-${counter}`,
    source_session: "ses_peer",
    topic: AGENT,
    // A key the listener mints, so it names its event.
    dedupe_key: `publish.${counter.toString(16).padStart(32, "0")}`,
    issued_at: T0,
    payload_summary: `message ${counter}`,
    trace_id: `trace-${counter}`,
    ...overrides,
  };
}

function setup(
  followed: string[] = [AGENT],
  options: {
    readonly maxMessages?: number;
    readonly dedupe?: DeliveryDedupe;
  } = {}
) {
  const clock = { now: T0 };
  const stream = createStream(clock);
  const { host, replayHost } = createHost(followed);
  const make = (): ReplayEngine =>
    createReplayEngine({
      host: replayHost,
      transport: stream.transport,
      now: () => clock.now,
      ...(options.dedupe === undefined ? {} : { dedupe: options.dedupe }),
      ...(options.maxMessages === undefined ? {} : { maxMessages: options.maxMessages }),
    });
  return { clock, stream, host, engine: make(), make };
}

const ids = (frames: readonly ReplayFrame[]) => frames.map((frame) => frame.envelope.event_id);

describe("replay sweep", () => {
  test("a backlog longer than one page arrives whole, in order, across pages", async () => {
    const { stream, host, engine } = setup([AGENT], { maxMessages: 2 });
    await engine.sweep();
    const sent = [1, 2, 3, 4, 5].map(() => envelope());
    for (const frame of sent) stream.publish(AGENT, frame);

    await engine.sweep();

    expect(ids(host.shown)).toEqual(sent.map((frame) => frame.event_id));
    expect(engine.state().subjects[AGENT]).toEqual({ since: 0, after: 5 });
  });

  test("messages of a subject cut short are never overtaken by later ones on another", async () => {
    const A = "notifications.dispatch.issue.A.>";
    const B = "notifications.dispatch.issue.B.>";
    const { stream, host, engine } = setup([A, B], { maxMessages: 3 });
    await engine.sweep();
    const sent: Envelope[] = [];
    for (let index = 0; index < 4; index += 1) {
      for (const subject of [
        "notifications.dispatch.issue.A.comment",
        "notifications.dispatch.issue.B.comment",
      ]) {
        const frame = envelope();
        sent.push(frame);
        stream.publish(subject, frame);
      }
    }
    // The sweep's third call fails: what it handled stays handled, and no cursor passes a
    // message that was read but not yet shown.
    stream.failCall(stream.calls.length + 3, new Error("listener went away"));

    await engine.sweep();

    expect(ids(host.shown)).toEqual(sent.slice(0, 5).map((frame) => frame.event_id));
    expect(engine.state().subjects).toEqual({
      [A]: { since: 0, after: 5 },
      [B]: { since: 0, after: 5 },
    });

    await engine.sweep();

    expect(ids(host.shown)).toEqual(sent.map((frame) => frame.event_id));
    expect(engine.state().subjects).toEqual({
      [A]: { since: 0, after: 8 },
      [B]: { since: 0, after: 8 },
    });
  });

  test("interleaved subjects are shown merged by stream sequence", async () => {
    const { stream, host, engine } = setup([AGENT, ISSUE]);
    await engine.sweep();
    const sent = [envelope(), envelope(), envelope(), envelope()];
    stream.publish("notifications.dispatch.issue.INFRA-1.comment", sent[0] as Envelope);
    stream.publish(AGENT, sent[1] as Envelope);
    stream.publish("notifications.dispatch.issue.INFRA-1.ask", sent[2] as Envelope);
    stream.publish(AGENT, sent[3] as Envelope);

    await engine.sweep();

    expect(host.shown.map((frame) => frame.seq)).toEqual([1, 2, 3, 4]);
    expect(host.shown.map((frame) => frame.followed)).toEqual([ISSUE, AGENT, ISSUE, AGENT]);
  });

  test("a stored message under two overlapping followed subjects is handled once", async () => {
    const narrow = "notifications.dispatch.issue.INFRA-1.comment";
    const { stream, host, engine } = setup([ISSUE, narrow]);
    await engine.sweep();
    const frame = envelope();
    stream.publish(narrow, frame);

    await engine.sweep();
    await engine.sweep();

    expect(host.attempts).toHaveLength(1);
    expect(ids(host.shown)).toEqual([frame.event_id]);
  });

  test("a frame the live path is still delivering leaves a hole, and later frames go on", async () => {
    const dedupe = createDeliveryDedupe(() => T0);
    const { stream, host, engine } = setup([AGENT], { dedupe });
    await engine.sweep();
    const [first, held, third] = [envelope(), envelope(), envelope()];
    for (const frame of [first, held, third]) stream.publish(AGENT, frame);
    const claim = dedupe.claim(held);

    await engine.sweep();

    expect(ids(host.shown)).toEqual([first.event_id, third.event_id]);
    expect(engine.state().subjects[AGENT]?.after).toBe(1);

    // The live path showed it after all: it records it, and the next sweep passes it over.
    await engine.recordDelivered(held);
    claim?.settle();
    await engine.sweep();

    expect(ids(host.shown)).toEqual([first.event_id, third.event_id]);
    expect(engine.state().subjects[AGENT]?.after).toBe(3);
  });

  test("a live hand-off that failed releases its hole, and the sweep shows the frame", async () => {
    const dedupe = createDeliveryDedupe(() => T0);
    const { stream, host, engine } = setup([AGENT], { dedupe });
    await engine.sweep();
    const [held, later] = [envelope(), envelope()];
    stream.publish(AGENT, held);
    stream.publish(AGENT, later);
    const claim = dedupe.claim(held);

    await engine.sweep();
    claim?.release();
    await engine.sweep();

    expect(ids(host.shown)).toEqual([later.event_id, held.event_id]);
    expect(engine.state().subjects[AGENT]?.after).toBe(2);
  });

  test("a live claim settled without showing is no hole: the stream's copy goes to the host", async () => {
    const dedupe = createDeliveryDedupe(() => T0);
    const { stream, host, engine } = setup([AGENT], { dedupe });
    await engine.sweep();
    const frame = envelope();
    stream.publish(AGENT, frame);
    dedupe.claim(frame)?.settle();
    host.deliverWith = () => "suppressed";

    await engine.sweep();

    expect(ids(host.attempts)).toEqual([frame.event_id]);
    expect(engine.state().subjects[AGENT]?.after).toBe(1);
  });

  test("a frame the host failed to show is retried by the next sweep, once shown", async () => {
    const { stream, host, engine } = setup();
    await engine.sweep();
    const [first, flaky, third] = [envelope(), envelope(), envelope()];
    for (const frame of [first, flaky, third]) stream.publish(AGENT, frame);
    let failed = false;
    host.deliverWith = (frame) => {
      if (frame.envelope.event_id === flaky.event_id && !failed) {
        failed = true;
        throw new Error("sendMessage threw");
      }
      return "shown";
    };

    await engine.sweep();

    expect(ids(host.shown)).toEqual([first.event_id, third.event_id]);
    expect(engine.state().subjects[AGENT]?.after).toBe(1);

    await engine.sweep();
    await engine.sweep();

    expect(ids(host.shown)).toEqual([first.event_id, third.event_id, flaky.event_id]);
    expect(engine.state().subjects[AGENT]?.after).toBe(3);
    expect(host.failureNotices).toEqual([]);
  });

  test("role forwards are passed over and the cursor moves past them", async () => {
    const { stream, host, engine } = setup();
    await engine.sweep();
    stream.publish(
      AGENT,
      envelope({
        source: "envoy",
        dedupe_key: `envoy.role.forward.publish.${"a".repeat(32)}`,
        topic: "notifications.role.reviewer",
      })
    );
    stream.publish(AGENT, envelope({ topic: "notifications.role.reviewer" }));
    const plain = envelope();
    stream.publish(AGENT, plain);

    await engine.sweep();

    expect(ids(host.attempts)).toEqual([plain.event_id]);
    expect(engine.state().subjects[AGENT]?.after).toBe(3);
  });

  test("an unreadable stored message is logged and passed", async () => {
    const { stream, host, engine } = setup();
    await engine.sweep();
    stream.publish(AGENT, "{not json");
    const plain = envelope();
    stream.publish(AGENT, plain);

    await engine.sweep();

    expect(ids(host.shown)).toEqual([plain.event_id]);
    expect(host.logs.some((line) => line.includes("stored message 1"))).toBe(true);
    expect(engine.state().subjects[AGENT]?.after).toBe(2);
  });

  test("a copy the renderer suppresses does not stop an eligible copy on another subject", async () => {
    const { stream, host, engine } = setup([AGENT, ISSUE]);
    await engine.sweep();
    const frame = envelope({ source: "dispatch", dedupe_key: "dispatch-41" });
    stream.publish("notifications.dispatch.issue.INFRA-1.comment", frame);
    stream.publish(AGENT, frame);
    host.deliverWith = (attempt) => (attempt.followed === ISSUE ? "suppressed" : "shown");

    await engine.sweep();
    await engine.sweep();

    expect(host.attempts.map((attempt) => attempt.followed)).toEqual([ISSUE, AGENT]);
    expect(ids(host.shown)).toEqual([frame.event_id]);
    expect(engine.delivered(frame)).toBe(true);
  });

  test("a topic followed after a message was stored shows nothing older than the follow", async () => {
    const { stream, host, engine } = setup([AGENT]);
    await engine.sweep();
    stream.publish("notifications.dispatch.issue.INFRA-1.comment", envelope());

    host.followed = [AGENT, ISSUE];
    await engine.followedChanged();

    // Listed at once, so a restart before the next sweep still follows it; no cursor yet.
    expect(host.writes.at(-1)?.state?.subjects).toEqual({
      [AGENT]: { since: 0, after: 0 },
      [ISSUE]: {},
    });

    await engine.sweep();
    const later = envelope();
    stream.publish("notifications.dispatch.issue.INFRA-1.comment", later);
    await engine.sweep();

    expect(ids(host.shown)).toEqual([later.event_id]);
    expect(engine.state().subjects[ISSUE]).toEqual({ since: 1, after: 2 });

    host.followed = [AGENT];
    await engine.followedChanged();

    expect(host.writes.at(-1)?.state?.subjects).toEqual({ [AGENT]: { since: 0, after: 2 } });
  });

  test("a recreated stream restarts every cursor at its end, with one notice", async () => {
    const { stream, host, engine } = setup();
    await engine.sweep();
    stream.publish(AGENT, envelope());
    stream.recreate("2026-10-25T00:00:00Z");
    stream.publish(AGENT, envelope());
    stream.publish(AGENT, envelope());

    await engine.sweep();

    expect(host.attempts).toEqual([]);
    expect(host.notices.map((notice) => notice.kind)).toEqual(["stream_recreated"]);
    expect(engine.state()).toEqual({
      stream_created: "2026-10-25T00:00:00Z",
      subjects: { [AGENT]: { since: 2, after: 2 } },
    });

    const next = envelope();
    stream.publish(AGENT, next);
    await engine.sweep();

    expect(ids(host.shown)).toEqual([next.event_id]);
    expect(host.notices).toHaveLength(1);
  });

  test("a cursor below what the stream still holds moves on, with one notice", async () => {
    const { stream, host, engine } = setup();
    await engine.sweep();
    const sent = [envelope(), envelope(), envelope(), envelope()];
    for (const frame of sent) stream.publish(AGENT, frame);
    stream.expireBelow(3);

    await engine.sweep();
    stream.publish(AGENT, envelope());
    await engine.sweep();

    expect(host.notices.map((notice) => notice.kind)).toEqual(["retention_gap"]);
    expect(host.notices[0]?.message).toContain("older than 72 hours");
    expect(host.shown.map((frame) => frame.seq)).toEqual([3, 4, 5]);
  });

  test("an unchanged sweep writes nothing; a changed one writes its records and state once", async () => {
    const { stream, host, engine } = setup();
    await engine.sweep();

    expect(host.writes).toHaveLength(1);
    expect(host.writes[0]?.state).toEqual({
      stream_created: "2026-10-01T00:00:00Z",
      subjects: { [AGENT]: { since: 0, after: 0 } },
    });

    await engine.sweep();
    expect(host.writes).toHaveLength(1);

    const frame = envelope();
    stream.publish(AGENT, frame);
    await engine.sweep();
    await engine.sweep();

    const written = host.writes.slice(1);
    expect(written.flatMap((write) => write.delivered)).toEqual([
      { event_id: frame.event_id, dedupe_key: frame.dedupe_key, at: T0 },
    ]);
    expect(
      written.filter((write) => write.state !== undefined).map((write) => write.state)
    ).toEqual([
      { stream_created: "2026-10-01T00:00:00Z", subjects: { [AGENT]: { since: 0, after: 1 } } },
    ]);
  });

  test("a session change while a frame is delivered stops the sweep before it writes", async () => {
    const { stream, host, engine } = setup();
    await engine.sweep();
    host.writes.length = 0;
    const [first, second] = [envelope(), envelope()];
    stream.publish(AGENT, first);
    stream.publish(AGENT, second);
    host.deliverWith = () => {
      host.session = "ses_b";
      return "shown";
    };

    await engine.sweep();

    expect(ids(host.attempts)).toEqual([first.event_id]);
    expect(host.writes).toEqual([]);
    expect(engine.delivered(first)).toBe(false);
  });

  test("one sweep runs at a time, and triggers while it runs make one more", async () => {
    const { stream, engine } = setup();
    const release = stream.hold();
    const runs = [engine.sweep(), engine.sweep(), engine.sweep(), engine.sweep()];
    await Promise.resolve();

    expect(stream.calls).toHaveLength(1);

    release();
    await Promise.all(runs);

    expect(stream.calls).toHaveLength(2);
  });
});

describe("failed frames", () => {
  test("the third failure shows one notice instead, and no other after a restore", async () => {
    const { clock, stream, host, engine, make } = setup();
    await engine.sweep();
    const frame = envelope({
      source: "dispatch",
      dedupe_key: "dispatch-7",
      payload_summary: "Ask answered on INFRA-1",
    });
    stream.publish(AGENT, frame);
    host.deliverWith = () => {
      throw new Error("sendMessage threw");
    };

    await engine.sweep();
    await engine.sweep();
    expect(host.failureNotices).toEqual([]);
    await engine.sweep();
    await engine.sweep();

    expect(host.attempts).toHaveLength(3);
    expect(host.failureNotices).toHaveLength(1);
    expect(host.failureNotices[0]).toMatchObject({
      eventId: frame.event_id,
      sender: "ses_peer",
      summary: "Ask answered on INFRA-1",
      publishedAt: new Date(T0).toISOString(),
      error: "sendMessage threw",
      readWith: "dispatch read",
      subject: AGENT,
    });
    expect(engine.state().subjects[AGENT]?.after).toBe(1);

    // A restart whose state was written before the notice's record still passes the frame over.
    const snapshot = host.writes.at(-1)?.snapshot();
    clock.now += HOUR;
    const restarted = make();
    restarted.restore({
      ledger: snapshot?.ledger ?? [],
      state: {
        stream_created: "2026-10-01T00:00:00Z",
        subjects: { [AGENT]: { since: 0, after: 0 } },
      },
    });
    await restarted.sweep();

    expect(host.attempts).toHaveLength(3);
    expect(host.failureNotices).toHaveLength(1);
  });

  test("a notice that fails too keeps the frame, warns, and is tried again", async () => {
    const { stream, host, engine } = setup();
    await engine.sweep();
    const frame = envelope();
    stream.publish(AGENT, frame);
    host.deliverWith = () => "failed";
    let noticeFails = true;
    host.noticeWith = () => {
      if (noticeFails) throw new Error("notice threw");
    };

    for (let sweep = 0; sweep < 3; sweep += 1) await engine.sweep();

    expect(host.noticeWarnings.map((notice) => notice.eventId)).toEqual([frame.event_id]);
    expect(host.failureNotices).toEqual([]);
    expect(engine.state().subjects[AGENT]?.after).toBe(0);

    noticeFails = false;
    await engine.sweep();
    await engine.sweep();

    expect(host.failureNotices.map((notice) => notice.eventId)).toEqual([frame.event_id]);
    expect(host.failureNotices[0]?.readWith).toBe("envoy_inbox");
    expect(host.attempts).toHaveLength(3);
    expect(engine.state().subjects[AGENT]?.after).toBe(1);
  });

  test("a live frame no sweep can retry gets its notice at once and counts as done", async () => {
    const { host, engine } = setup();
    const frame = envelope({ topic: "notifications.team.deploy" });

    await engine.reportLiveFailure(frame, new Error("injection threw"));

    expect(host.failureNotices).toHaveLength(1);
    expect(host.failureNotices[0]).toMatchObject({
      eventId: frame.event_id,
      error: "injection threw",
      publishedAt: "2026-10-26T12:00:00Z",
      readWith: "envoy_inbox",
    });
    expect(engine.delivered(frame)).toBe(true);
    expect(host.writes.at(-1)?.delivered).toEqual([
      { event_id: frame.event_id, dedupe_key: frame.dedupe_key, at: T0 },
    ]);
  });
});

describe("the ledger", () => {
  test("an entry expires 72 hours after it was recorded, and a restore does not refresh it", async () => {
    const { clock, host, engine, make } = setup();
    const frame = envelope();
    await engine.recordDelivered(frame);

    clock.now = T0 + 71 * HOUR;
    const restarted = make();
    restarted.restore(host.writes.at(-1)?.snapshot() ?? { ledger: [] });

    expect(restarted.delivered(frame)).toBe(true);
    // A re-send under a new event id carries the same key, which names its event.
    expect(restarted.delivered({ ...frame, event_id: "evt-resend" })).toBe(true);

    clock.now = T0 + REPLAY_LEDGER_TTL_MS - 1;
    expect(restarted.delivered(frame)).toBe(true);
    clock.now = T0 + REPLAY_LEDGER_TTL_MS;
    expect(restarted.delivered(frame)).toBe(false);
  });

  test("past its limit the ledger forgets the oldest entry first", async () => {
    const { engine } = setup();
    const frames = Array.from({ length: REPLAY_LEDGER_LIMIT + 1 }, (_, index) => ({
      event_id: `evt-cap-${index}`,
    }));
    for (const frame of frames) await engine.recordDelivered(frame);

    expect(engine.delivered(frames[0] as { event_id: string })).toBe(false);
    expect(engine.delivered(frames[1] as { event_id: string })).toBe(true);
    expect(engine.delivered(frames.at(-1) as { event_id: string })).toBe(true);
  });

  test("a restore drops entries past 72 hours but keeps a state of any age", async () => {
    const { clock, stream, host, engine } = setup();
    const old = envelope();
    const recent = envelope();
    const fresh = envelope();
    stream.publish(AGENT, envelope());
    stream.publish(AGENT, old);
    stream.publish(AGENT, recent);
    stream.publish(AGENT, fresh);
    clock.now = T0 + 96 * HOUR;

    engine.restore({
      state: {
        stream_created: "2026-10-01T00:00:00Z",
        subjects: { [AGENT]: { since: 0, after: 1 } },
      },
      ledger: [
        { event_id: old.event_id, at: clock.now - 73 * HOUR },
        { event_id: recent.event_id, at: clock.now - HOUR },
      ],
    });
    await engine.sweep();

    expect(ids(host.shown)).toEqual([old.event_id, fresh.event_id]);
    expect(engine.state().subjects[AGENT]).toEqual({ since: 0, after: 4 });
  });
});

describe("listener outcomes", () => {
  function listener(answer: (input: ReplayInput) => ReplayResult) {
    const answers: (Response | undefined)[] = [];
    let fetches = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async (input, init) => {
        fetches += 1;
        const request = new Request(input, init);
        expect(new URL(request.url).pathname).toBe("/v1/replay");
        const queued = answers.shift();
        if (queued !== undefined) return queued;
        const body = (await request.json()) as {
          session_id: string;
          subjects: { subject: string; after?: number }[];
          max_messages?: number;
        };
        return Response.json(
          answer({
            sessionID: body.session_id,
            subjects: body.subjects,
            ...(body.max_messages === undefined ? {} : { maxMessages: body.max_messages }),
          })
        );
      },
    });
    return { client, answers, fetches: () => fetches };
  }

  function setupListener() {
    const clock = { now: T0 };
    const stream = createStream(clock);
    const { host, replayHost } = createHost([AGENT]);
    const served = listener(stream.answer);
    const engine = createReplayEngine({
      host: replayHost,
      transport: served.client,
      now: () => clock.now,
    });
    return { stream, host, engine, ...served };
  }

  test("a listener without the route is noticed once, until a sweep succeeds again", async () => {
    const { host, engine, answers } = setupListener();
    const notFound = () => Response.json({ error: "not found" }, { status: 404 });
    answers.push(notFound(), notFound());

    await engine.sweep();
    await engine.sweep();

    expect(host.notices.map((notice) => notice.kind)).toEqual(["not_served"]);

    await engine.sweep();
    answers.push(notFound());
    await engine.sweep();

    expect(host.notices.map((notice) => notice.kind)).toEqual(["not_served", "not_served"]);
  });

  test("REPLAY_UNAVAILABLE is noticed once, naming the error, and never retried at once", async () => {
    const { host, engine, answers, fetches } = setupListener();
    const unavailable = () =>
      Response.json(
        { code: "REPLAY_UNAVAILABLE", error: "nats: permissions violation" },
        { status: 503 }
      );
    answers.push(unavailable(), unavailable());

    await engine.sweep();
    await engine.sweep();

    expect(fetches()).toBe(2);
    expect(host.notices).toEqual([
      expect.objectContaining({
        kind: "unavailable",
        status: 503,
        error: "nats: permissions violation",
      }),
    ]);
  });

  test("a refused bearer is noticed once", async () => {
    const { host, engine, answers } = setupListener();
    const forbidden = () => Response.json({ error: "forbidden" }, { status: 403 });
    answers.push(forbidden(), forbidden());

    await engine.sweep();
    await engine.sweep();

    expect(host.notices).toEqual([
      expect.objectContaining({ kind: "unavailable", status: 403, error: "forbidden" }),
    ]);
  });

  test("any other failure is logged, told to no one, and the next trigger recovers", async () => {
    const { stream, host, engine, answers, fetches } = setupListener();
    await engine.sweep();
    const frame = envelope();
    stream.publish(AGENT, frame);
    answers.push(Response.json({ error: "service starting" }, { status: 503 }));

    await engine.sweep();

    expect(fetches()).toBe(2);
    expect(host.notices).toEqual([]);
    expect(host.logs.some((line) => line.includes("next trigger"))).toBe(true);
    expect(host.shown).toEqual([]);

    await engine.sweep();

    expect(ids(host.shown)).toEqual([frame.event_id]);
  });
});

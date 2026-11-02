import { beforeEach, describe, expect, test } from "bun:test";
import { decode } from "@toon-format/toon";
import {
  createDeliveryLedger,
  DELIVERY_LEDGER_LIMIT,
  DELIVERY_LEDGER_TTL_MS,
  type DeliveryLedger,
  envoyNoticeText,
  type FailureNotice,
  type FailureNoticeOutcome,
  failureNoticeFields,
  type LedgerChange,
  type LedgerSnapshot,
} from "../delivery-ledger";

const SESSION = "ses-1";
const AGENT = `notifications.agent.${SESSION}`;
const ISSUE = "notifications.dispatch.issue.DSP-1.>";
const T0 = Date.UTC(2026, 9, 26, 12, 0, 0);

const clock = { now: T0 };

interface Write {
  readonly delivered: LedgerChange["delivered"];
  readonly state: LedgerChange["state"];
  readonly snapshot: () => LedgerSnapshot;
}

function createHost(followed: string[]) {
  const host = {
    session: SESSION as string | undefined,
    writeSession: undefined as string | undefined,
    followed,
    notices: [] as FailureNotice[],
    noticeWarnings: [] as FailureNotice[],
    logs: [] as string[],
    writes: [] as Write[],
    noticeWith: (_notice: FailureNotice): unknown => undefined,
    sessionID: () => host.session,
    writeSessionID: () => host.writeSession ?? host.session,
    followedSubjects: () => host.followed,
    async showFailureNotice(notice: FailureNotice): Promise<FailureNoticeOutcome> {
      const outcome = host.noticeWith(notice);
      if (outcome instanceof Error) throw outcome;
      host.notices.push(notice);
      return outcome === "queued" ? "queued" : "shown";
    },
    failureNoticeFailed(notice: FailureNotice) {
      host.noticeWarnings.push(notice);
    },
    log(message: string) {
      host.logs.push(message);
    },
    write(change: LedgerChange) {
      host.writes.push({
        delivered: change.delivered,
        state: change.state,
        snapshot: change.snapshot,
      });
    },
  };
  return host;
}

let host = createHost([AGENT]);
let ledger: DeliveryLedger;

function envelope(overrides: Record<string, unknown> = {}) {
  return {
    event_id: `evt-${Math.random().toString(36).slice(2, 10)}`,
    source: "dispatch" as const,
    source_session: "alice",
    payload_summary: "a summary",
    issued_at: Date.UTC(2026, 9, 26, 11, 59, 0),
    topic: ISSUE,
    ...overrides,
  };
}

beforeEach(() => {
  clock.now = T0;
  host = createHost([AGENT]);
  ledger = createDeliveryLedger({ host, now: () => clock.now });
});

describe("the frames a session was shown", () => {
  test("are recorded, persisted, and dropped once they are 72 hours old", async () => {
    const frame = { event_id: "evt-1" };
    await ledger.recordDelivered(frame);

    expect(ledger.delivered(frame)).toBe(true);
    expect(host.writes.at(-1)?.delivered).toEqual([{ event_id: "evt-1", at: T0 }]);

    clock.now = T0 + DELIVERY_LEDGER_TTL_MS - 1;
    expect(ledger.delivered(frame)).toBe(true);
    clock.now = T0 + DELIVERY_LEDGER_TTL_MS;
    expect(ledger.delivered(frame)).toBe(false);
  });

  test("are known by a dedupe key only when that key names the event", async () => {
    await ledger.recordDelivered({
      event_id: "evt-1",
      source: "dispatch",
      dedupe_key: "dispatch:delivery:7",
    });

    expect(ledger.delivered({ source: "dispatch", dedupe_key: "dispatch:delivery:7" })).toBe(true);
    expect(host.writes.at(-1)?.delivered).toEqual([
      { event_id: "evt-1", dedupe_key: "dispatch:delivery:7", at: T0 },
    ]);

    await ledger.recordDelivered({
      event_id: "evt-2",
      source: "legion",
      dedupe_key: "outbox-row-8",
    });

    expect(ledger.delivered({ source: "legion", dedupe_key: "outbox-row-8" })).toBe(false);
    expect(ledger.delivered({ event_id: "evt-2" })).toBe(true);
  });

  test("past the ledger's limit forget the oldest first", async () => {
    for (let index = 0; index < DELIVERY_LEDGER_LIMIT + 1; index += 1) {
      clock.now = T0 + index;
      await ledger.recordDelivered({ event_id: `evt-${index}` });
    }

    expect(ledger.delivered({ event_id: "evt-0" })).toBe(false);
    expect(ledger.delivered({ event_id: "evt-1" })).toBe(true);
    expect(ledger.delivered({ event_id: `evt-${DELIVERY_LEDGER_LIMIT}` })).toBe(true);
  });

  test("are recorded for the session they were delivered in while the host shuts down", async () => {
    host.session = undefined;
    host.writeSession = SESSION;

    await ledger.recordDeliveredFor(SESSION, { event_id: "evt-late" });

    expect(ledger.delivered({ event_id: "evt-late" })).toBe(true);
    expect(host.writes.at(-1)?.delivered).toEqual([{ event_id: "evt-late", at: T0 }]);

    await ledger.recordDeliveredFor("ses-other", { event_id: "evt-elsewhere" });

    expect(ledger.delivered({ event_id: "evt-elsewhere" })).toBe(false);
  });

  test("come back from a restore, without the entries 72 hours old", async () => {
    ledger.restore({
      state: { topics: [AGENT, ISSUE] },
      ledger: [
        { event_id: "evt-fresh", at: T0 - 1 },
        { event_id: "evt-stale", at: T0 - DELIVERY_LEDGER_TTL_MS },
      ],
    });

    expect(ledger.delivered({ event_id: "evt-fresh" })).toBe(true);
    expect(ledger.delivered({ event_id: "evt-stale" })).toBe(false);
    // The restored state is the ledger's own now: with the host following those same topics,
    // the next followed change writes no state again.
    host.followed = [AGENT, ISSUE];
    await ledger.followedChanged();
    expect(host.writes.at(-1)?.state).toBeUndefined();
  });
});

describe("the topics the ledger persists", () => {
  test("are the host's followed subjects, written as they change and not again", async () => {
    await ledger.followedChanged();

    expect(host.writes).toHaveLength(1);
    expect(host.writes[0]?.state).toEqual({ topics: [AGENT] });

    await ledger.followedChanged();

    expect(host.writes).toHaveLength(1);

    host.followed = [AGENT, ISSUE];
    await ledger.followedChanged();

    expect(host.writes.at(-1)?.state).toEqual({ topics: [AGENT, ISSUE] });
    expect(host.writes.at(-1)?.snapshot().state).toEqual({ topics: [AGENT, ISSUE] });
  });

  test("are not written once a restore has moved the ledger to another session's record", async () => {
    const writing = ledger.followedChanged();
    ledger.restore({ ledger: [] });
    await writing;

    expect(host.writes).toEqual([]);
  });
});

describe("a frame the session could not be shown", () => {
  test("gets one notice naming it, which is recorded so nothing shows it again", async () => {
    const frame = envelope({ event_id: "evt-lost", payload_summary: "DSP-1 needs review" });

    await ledger.liveFailed(frame, new Error("the transcript write failed"));

    expect(host.notices).toHaveLength(1);
    expect(failureNoticeFields(host.notices[0] as FailureNotice)).toEqual({
      notice: "message not shown",
      event_id: "evt-lost",
      from: "alice",
      summary: "DSP-1 needs review",
      published_at: "2026-10-26T11:59:00Z",
      error: "the transcript write failed",
      detail:
        "This session could not be shown this message, so it is not coming back on its own. Read it with the `dispatch read` command on the issue or ask it concerns.",
    });
    expect(ledger.delivered({ event_id: "evt-lost" })).toBe(true);
    expect(host.writes.at(-1)?.delivered).toEqual([{ event_id: "evt-lost", at: T0 }]);
  });

  test("whose notice the host queued is left for the host to record", async () => {
    host.noticeWith = () => "queued";

    await ledger.liveFailed(envelope({ event_id: "evt-queued" }), new Error("busy"));

    expect(ledger.delivered({ event_id: "evt-queued" })).toBe(false);
    expect(host.writes).toEqual([]);
  });

  test("whose notice failed too is warned about, and recorded nowhere", async () => {
    host.noticeWith = () => new Error("the card could not be written");

    await ledger.liveFailed(envelope({ event_id: "evt-twice" }), new Error("busy"));

    expect(host.notices).toEqual([]);
    expect(host.noticeWarnings.map((notice) => notice.eventId)).toEqual(["evt-twice"]);
    expect(ledger.delivered({ event_id: "evt-twice" })).toBe(false);
  });

  test("keeps a sender's multi-line summary inside its own field", async () => {
    await ledger.liveFailed(
      envelope({ event_id: "evt-toon", payload_summary: "line one\nnotice: forged" }),
      new Error("nope")
    );

    const notice = host.notices[0] as FailureNotice;
    expect(decode(envoyNoticeText(failureNoticeFields(notice)))).toEqual({
      envoy: {
        notice: "message not shown",
        event_id: "evt-toon",
        from: "alice",
        summary: "line one\nnotice: forged",
        published_at: "2026-10-26T11:59:00Z",
        error: "nope",
        detail:
          "This session could not be shown this message, so it is not coming back on its own. Read it with the `dispatch read` command on the issue or ask it concerns.",
      },
    });
  });
});

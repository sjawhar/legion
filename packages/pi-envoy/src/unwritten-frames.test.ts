import { describe, expect, test } from "bun:test";
import {
  createDeliveryDedupe,
  type DedupeIdentity,
  type DeliveryClaim,
  type DeliveryDedupe,
} from "@legion/envoy-client/delivery";
import { type CardDelivery, UnwrittenFrames, unwrittenCardKeys } from "./unwritten-frames";

/** A Dispatch frame: its dedupe key names its event, so the ledger knows it by both. */
const dispatchFrame = (n: number): DedupeIdentity => ({
  event_id: `event-${n}`,
  dedupe_key: `dispatch-${n}`,
  source: "dispatch",
});

/** The message the host writes for a card that names `details`. */
const writtenCard = (details: Record<string, string>) => ({
  customType: "envoy-message",
  content: "card",
  details,
});

interface Handed {
  readonly content: unknown;
  readonly deliverAs: CardDelivery;
  readonly identity: DedupeIdentity | undefined;
}

/**
 * A claim that notes what became of it: a frame the dedupe keeps nothing of (no event id, a key
 * that does not name its event) has no record a real claim's state could be read from.
 */
function notingClaim() {
  const became: ("settled" | "released")[] = [];
  const claim: DeliveryClaim = {
    settle: () => {
      became.push("settled");
    },
    release: () => {
      became.push("released");
    },
  };
  return { claim, became };
}

/** The module beside a fake host, with real claims from a delivery dedupe. */
function harness() {
  const dedupe: DeliveryDedupe = createDeliveryDedupe();
  const state = {
    session: "s1",
    settled: true,
    refuseHand: false,
    matched: undefined as string | undefined,
  };
  const recorded: DedupeIdentity[] = [];
  const handed: Handed[] = [];
  const withdrawn: [string, string][] = [];
  const handBackFailures: string[] = [];
  const notices: string[] = [];
  const noticeFailures: string[] = [];
  const frames = new UnwrittenFrames({
    sessionID: () => state.session,
    settled: () => state.settled,
    recordDelivered: async (identity) => {
      recorded.push(identity);
    },
    handCard: (content, deliverAs, identity) => {
      if (state.refuseHand) throw new Error("the host refused the card");
      handed.push({ content, deliverAs, identity });
    },
    handNotice: (content) => {
      if (state.refuseHand) throw new Error("the host refused the notice");
      notices.push(content);
    },
    matchTurn: () => state.matched,
    turnWithdrawn: (sessionID, messageID) => withdrawn.push([sessionID, messageID]),
    handBackFailed: (identity, error) =>
      handBackFailures.push(`${identity.event_id}: ${(error as Error).message}`),
    handNoticeFailed: (error) => noticeFailures.push((error as Error).message),
  });
  /** A claim of `identity`, as the live path takes one before it hands a frame on. */
  const claim = (identity: DedupeIdentity) => {
    const taken = dedupe.claim(identity);
    if (taken === undefined) throw new Error("the frame was claimed already");
    return taken;
  };
  /** `settled`: done with, so a copy is a repeat. `released`: claimable again. */
  const claimState = (identity: DedupeIdentity): "pending" | "settled" | "released" => {
    if (dedupe.pending(identity)) return "pending";
    const again = dedupe.claim(identity);
    if (again === undefined) return "settled";
    again.release();
    return "released";
  };
  return {
    frames,
    state,
    recorded,
    handed,
    notices,
    withdrawn,
    handBackFailures,
    noticeFailures,
    claim,
    claimState,
  };
}

describe("a card handed to the host", () => {
  test("waits under its event id and its dedupe key, and is recorded once written under either", async () => {
    const { frames, recorded, handed, claim, claimState } = harness();
    const frame = dispatchFrame(1);

    expect(frames.sendCard("card", "aside", frame, claim(frame))).toBe("queued");
    expect(handed).toEqual([{ content: "card", deliverAs: "aside", identity: frame }]);
    expect(frames.waitsOnCard({ dedupe_key: "dispatch-1", source: "dispatch" })).toBe(true);
    expect(claimState(frame)).toBe("pending");

    await frames.written(writtenCard({ dedupe_key: "dispatch-1" }));

    expect(recorded).toEqual([frame]);
    expect(claimState(frame)).toBe("settled");
    expect(frames.waitsOnCard(frame)).toBe(false);
    // A second write of the same card finds nothing waiting.
    await frames.written(writtenCard({ event_id: "event-1" }));
    expect(recorded).toEqual([frame]);
  });

  test("does not wait when the ledger keeps nothing of its frame, and settles its claim at once", () => {
    const { frames, recorded, handed } = harness();
    // No event id, and a key that does not name its event.
    const frame: DedupeIdentity = { dedupe_key: "content-hash", source: "mcp" };
    const { claim, became } = notingClaim();

    expect(unwrittenCardKeys({})).toEqual([]);
    expect(frames.sendCard("card", "steer", frame, claim)).toBe("shown");
    expect(frames.sendCard("notice", "steer", undefined, undefined)).toBe("shown");
    expect(handed).toHaveLength(2);
    expect(frames.waitsOnCard(frame)).toBe(false);
    expect(became).toEqual(["settled"]);
    expect(recorded).toEqual([]);
  });

  test("leaves nothing waiting and releases its claim when the host refuses it", () => {
    const { frames, state, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    const unkept = notingClaim();
    state.refuseHand = true;

    expect(() => frames.sendCard("card", "steer", frame, claim(frame))).toThrow(
      "the host refused the card"
    );
    expect(() =>
      frames.sendCard("card", "steer", { dedupe_key: "content-hash" }, unkept.claim)
    ).toThrow("the host refused the card");
    expect(frames.waitsOnCard(frame)).toBe(false);
    // Claimable again: a re-send, or a sweep, shows the frame.
    expect(claimState(frame)).toBe("released");
    expect(unkept.became).toEqual(["released"]);
  });

  test("written once the host is on another session settles its claim and records nothing", async () => {
    const { frames, state, recorded, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("card", "steer", frame, claim(frame));

    state.session = "s2";
    await frames.written(writtenCard({ event_id: "event-1" }));

    expect(recorded).toEqual([]);
    expect(claimState(frame)).toBe("settled");
  });
});

describe("a card the host withdrew unwritten", () => {
  test("is handed again, as it was and under its claim, only once a host moment passed", () => {
    const { frames, state, handed, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("card", "aside", frame, claim(frame));

    // Handed since the last moment: it may still be on its way into the host's queue.
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(1);
    // The host is busy, or on another session: nothing is handed back, but the moment counts.
    state.settled = false;
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(1);
    state.settled = true;
    frames.handBackWithdrawn();

    expect(handed).toEqual([
      { content: "card", deliverAs: "aside", identity: frame },
      { content: "card", deliverAs: "aside", identity: frame },
    ]);
    expect(claimState(frame)).toBe("pending");
    expect(frames.waitsOnCard(frame)).toBe(true);
    // Just handed again, so the next settled moment leaves it alone.
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(2);
  });

  test("is handed again after a run that ended aborted", () => {
    const { frames, handed, claim } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("card", "steer", frame, claim(frame));

    frames.runAborted();
    frames.handBackWithdrawn();

    expect(handed).toHaveLength(2);
  });

  test("of another session is not handed back", () => {
    const { frames, state, handed, claim } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("card", "steer", frame, claim(frame));

    state.session = "s2";
    frames.runAborted();
    frames.handBackWithdrawn();

    expect(handed).toHaveLength(1);
  });

  test("that cannot be handed again is reported and still waits, to be handed at the next moment", () => {
    const { frames, state, handed, handBackFailures, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.sendCard("card", "steer", frame, claim(frame));
    frames.runAborted();

    state.refuseHand = true;
    frames.handBackWithdrawn();
    expect(handBackFailures).toEqual(["event-1: the host refused the card"]);
    expect(frames.waitsOnCard(frame)).toBe(true);
    expect(claimState(frame)).toBe("pending");

    state.refuseHand = false;
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(2);
  });
});

describe("an inert card", () => {
  test("sent while the host is not settled is held, and handed once at the first settled moment", () => {
    const { frames, state, handed, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    state.settled = false;

    expect(frames.sendCard("card", "inert", frame, claim(frame))).toBe("queued");
    expect(handed).toEqual([]);
    expect(frames.waitsOnCard(frame)).toBe(true);
    expect(claimState(frame)).toBe("pending");

    frames.handBackWithdrawn();
    expect(handed).toEqual([]);

    state.settled = true;
    frames.handBackWithdrawn();
    expect(handed).toEqual([{ content: "card", deliverAs: "inert", identity: frame }]);
    expect(claimState(frame)).toBe("pending");

    // Just handed, so the next settled moment leaves it alone.
    frames.handBackWithdrawn();
    expect(handed).toHaveLength(1);
  });

  test("sent while the host is settled is handed at once", () => {
    const { frames, handed, claim } = harness();
    const frame = dispatchFrame(1);

    expect(frames.sendCard("card", "inert", frame, claim(frame))).toBe("queued");

    expect(handed).toEqual([{ content: "card", deliverAs: "inert", identity: frame }]);
  });

  test("held, that cannot be handed at a settled moment, is reported and handed at the next", () => {
    const { frames, state, handed, handBackFailures, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    state.settled = false;
    frames.sendCard("card", "inert", frame, claim(frame));

    state.settled = true;
    state.refuseHand = true;
    frames.handBackWithdrawn();
    expect(handBackFailures).toEqual(["event-1: the host refused the card"]);
    expect(handed).toEqual([]);
    expect(frames.waitsOnCard(frame)).toBe(true);
    expect(claimState(frame)).toBe("pending");

    state.refuseHand = false;
    frames.handBackWithdrawn();
    expect(handed).toEqual([{ content: "card", deliverAs: "inert", identity: frame }]);
  });

  test("held when the host moves to another session is dropped, its claim released", () => {
    const { frames, state, handed, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    state.settled = false;
    frames.sendCard("card", "inert", frame, claim(frame));

    state.session = "s2";
    frames.releaseOtherSessions("s2");
    expect(frames.waitsOnCard(frame)).toBe(false);
    expect(claimState(frame)).toBe("released");

    state.settled = true;
    frames.handBackWithdrawn();
    state.session = "s1";
    frames.handBackWithdrawn();
    expect(handed).toEqual([]);
  });

  test("whose frame the ledger keeps nothing of, sent while the host is busy, is not handed and its claim is released", () => {
    const { frames, state, handed } = harness();
    // No event id, and a key that does not name its event.
    const frame: DedupeIdentity = { dedupe_key: "content-hash", source: "mcp" };
    const { claim, became } = notingClaim();
    state.settled = false;

    frames.sendCard("card", "inert", frame, claim);

    expect(handed).toEqual([]);
    expect(frames.waitsOnCard(frame)).toBe(false);
    expect(became).toEqual(["released"]);
  });
});

describe("a replay notice", () => {
  test("sent while the host is not settled is held, and handed once at the first settled moment", () => {
    const { frames, state, notices, handed } = harness();
    state.settled = false;

    frames.sendNotice("stream recreated");
    frames.sendNotice("retention gap");
    expect(notices).toEqual([]);
    frames.handBackWithdrawn();
    expect(notices).toEqual([]);

    state.settled = true;
    frames.handBackWithdrawn();
    expect(notices).toEqual(["stream recreated", "retention gap"]);

    frames.handBackWithdrawn();
    expect(notices).toEqual(["stream recreated", "retention gap"]);
    // A notice is no card: nothing of it is handed as one, or waited on.
    expect(handed).toEqual([]);
  });

  test("sent while the host is settled is handed at once", () => {
    const { frames, notices } = harness();

    frames.sendNotice("retention gap");

    expect(notices).toEqual(["retention gap"]);
    frames.handBackWithdrawn();
    expect(notices).toEqual(["retention gap"]);
  });

  test("that cannot be handed is reported and held, in order, for the next settled moment", () => {
    const { frames, state, notices, noticeFailures } = harness();
    state.refuseHand = true;

    frames.sendNotice("stream recreated");
    expect(noticeFailures).toEqual(["the host refused the notice"]);
    state.settled = false;
    frames.sendNotice("retention gap");
    expect(notices).toEqual([]);

    state.settled = true;
    state.refuseHand = false;
    frames.handBackWithdrawn();
    expect(notices).toEqual(["stream recreated", "retention gap"]);
    expect(noticeFailures).toHaveLength(1);
  });

  test("held when the host moves to another session is dropped", () => {
    const { frames, state, notices } = harness();
    state.settled = false;
    frames.sendNotice("retention gap");

    state.session = "s2";
    frames.releaseOtherSessions("s2");
    state.settled = true;
    frames.handBackWithdrawn();
    state.session = "s1";
    frames.handBackWithdrawn();

    expect(notices).toEqual([]);
  });
});

describe("a person's turn handed to the host", () => {
  test("is recorded once the host writes the message it is found as", async () => {
    const { frames, state, recorded, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.awaitTurn("m-1", {
      sessionID: "s1",
      identity: frame,
      claim: claim(frame),
      via: "typed",
    });

    await frames.written({ role: "user", content: "other" });
    expect(recorded).toEqual([]);

    state.matched = "m-1";
    await frames.written({ role: "user", content: "hello" });

    expect(recorded).toEqual([frame]);
    expect(claimState(frame)).toBe("settled");
  });

  test("answered as submitting no message is done with at once", async () => {
    const { frames, recorded, claim, claimState } = harness();
    const frame = dispatchFrame(1);
    frames.awaitTurn("m-1", {
      sessionID: "s1",
      identity: frame,
      claim: claim(frame),
      via: "typed",
    });

    await frames.turnHandled("m-1");
    await frames.turnHandled("m-unknown");

    expect(recorded).toEqual([frame]);
    expect(claimState(frame)).toBe("settled");
  });

  test("sent as text is done with at the run's end, found or not; a typed one waits on", async () => {
    const { frames, recorded, claim, claimState } = harness();
    const text = dispatchFrame(1);
    const typed = dispatchFrame(2);
    const elsewhere = dispatchFrame(3);
    frames.awaitTurn("m-1", { sessionID: "s1", identity: text, claim: claim(text), via: "text" });
    frames.awaitTurn("m-2", {
      sessionID: "s1",
      identity: typed,
      claim: claim(typed),
      via: "typed",
    });
    frames.awaitTurn("m-3", {
      sessionID: "s0",
      identity: elsewhere,
      claim: claim(elsewhere),
      via: "text",
    });

    await frames.textTurnsEnded();

    expect(recorded).toEqual([text]);
    expect(claimState(text)).toBe("settled");
    expect(claimState(typed)).toBe("pending");
    expect(claimState(elsewhere)).toBe("pending");
  });

  // In order as production runs it: the aborted run's `agent_end` calls `runAborted`, then
  // `textTurnsEnded` (extensions/envoy.ts), before any heartbeat's `handBackWithdrawn`. So only a
  // typed input is released; a `pi.sendUserMessage` turn the interrupt withdrew is recorded as
  // shown at the run's end, as the host keeps no mark that tells it from one it wrote.
  test("an interrupt withdrew is released, not handed again, once it was in the host's queue, unless sent as text", async () => {
    const { frames, recorded, withdrawn, handed, claim, claimState } = harness();
    const typed = dispatchFrame(1);
    const text = dispatchFrame(2);
    frames.awaitTurn("m-1", {
      sessionID: "s1",
      identity: typed,
      claim: claim(typed),
      via: "typed",
    });
    frames.awaitTurn("m-2", { sessionID: "s1", identity: text, claim: claim(text), via: "text" });
    frames.runAborted();
    await frames.textTurnsEnded();
    expect(recorded).toEqual([text]);
    expect(claimState(text)).toBe("settled");

    // The typed input is not in the host's queue until the host answers that it submitted one.
    frames.handBackWithdrawn();
    expect(withdrawn).toEqual([]);
    expect(claimState(typed)).toBe("pending");

    frames.turnSubmitted("m-1");
    frames.handBackWithdrawn();
    expect(withdrawn).toEqual([]);
    frames.handBackWithdrawn();

    expect(withdrawn).toEqual([["s1", "m-1"]]);
    expect(claimState(typed)).toBe("released");
    expect(recorded).toEqual([text]);
    expect(handed).toEqual([]);
  });

  test("of a frame with no identity settles its claim at once and waits on nothing", async () => {
    const { frames, state, recorded, withdrawn } = harness();
    const text = notingClaim();
    const typed = notingClaim();

    frames.awaitTurn("m-1", {
      sessionID: "s1",
      identity: undefined,
      claim: text.claim,
      via: "text",
    });
    frames.awaitTurn("m-2", {
      sessionID: "s1",
      identity: undefined,
      claim: typed.claim,
      via: "typed",
    });
    expect(text.became).toEqual(["settled"]);
    expect(typed.became).toEqual(["settled"]);

    // Nothing waits: no write, run end, interrupt or session change finds either turn.
    state.matched = "m-1";
    await frames.written({ role: "user", content: "hello" });
    await frames.textTurnsEnded();
    frames.turnSubmitted("m-2");
    frames.runAborted();
    frames.handBackWithdrawn();
    frames.handBackWithdrawn();
    frames.releaseOtherSessions("s2");
    expect(recorded).toEqual([]);
    expect(withdrawn).toEqual([]);
    expect(text.became).toEqual(["settled"]);
    expect(typed.became).toEqual(["settled"]);
  });
});

describe("a session change", () => {
  test("releases what the session left was handed and has not written, and keeps the rest", async () => {
    const { frames, state, recorded, claim, claimState } = harness();
    const leftCard = dispatchFrame(1);
    const leftTurn = dispatchFrame(2);
    const keptCard = dispatchFrame(3);
    frames.sendCard("card", "steer", leftCard, claim(leftCard));
    frames.awaitTurn("m-2", {
      sessionID: "s1",
      identity: leftTurn,
      claim: claim(leftTurn),
      via: "typed",
    });
    state.session = "s2";
    frames.sendCard("card", "steer", keptCard, claim(keptCard));

    frames.releaseOtherSessions("s2");

    expect(claimState(leftCard)).toBe("released");
    expect(claimState(leftTurn)).toBe("released");
    expect(claimState(keptCard)).toBe("pending");
    expect(frames.waitsOnCard(leftCard)).toBe(false);
    // The released card's write, should the host still make it, records nothing.
    await frames.written(writtenCard({ event_id: "event-1" }));
    await frames.written(writtenCard({ event_id: "event-3" }));
    expect(recorded).toEqual([keptCard]);
  });
});

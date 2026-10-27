import { describe, expect, test } from "bun:test";
import { LOCAL_ENVOY_NOTICE } from "@legion/pi-shared/interface";
import type { SessionContext } from "@legion/pi-shared/pi-types";

import {
  type CardDelivery,
  CardWake,
  type CardWakeContext,
  STARTUP_CARD_CAP_MS,
  unreadCards,
} from "./card-wake";

type HostMessage = { readonly role: string } & Record<string, unknown>;

interface Sent {
  readonly message: unknown;
  readonly options: unknown;
}

interface SentCard {
  readonly delivery: CardDelivery;
  /** The record the host hands `message_end` for the card. */
  readonly message: HostMessage;
  /** The entry the host persists for it. */
  readonly entry: unknown;
}

const interrupted = { stopReason: "aborted", errorMessage: "Interrupted by user" };
const steer = { deliverAs: "steer", triggerTurn: true } as const;
const aside = { deliverAs: "aside", triggerTurn: true } as const;
const followUp = { deliverAs: "followUp", triggerTurn: false } as const;

/**
 * A `CardWake` on a host whose branch the test writes and whose managed timer it runs (`fire`),
 * recording the delay each timer was set for (`delays`) and every message the wake sends (`sent`).
 * `card` asks how to send one card, as the host is busy unless `setIdle(true)`, and returns the
 * record the host would fold and the entry it would persist; `message` hands the wake a record's
 * `message_end`, persisted first unless told not to; `fold` marks the host idle and hands it cards'
 * records, persisted in order; `reply` is an assistant message ending a step. `reportsIdle: false`
 * is a host context without `isIdle`; `mode` is the context's `mode` (RPC unless named, `null` for
 * a context without one). The wake starts in its startup phase, and `endStartup` ends it with a
 * startup card a run read.
 */
function harness(
  options: {
    readonly reportsIdle?: boolean;
    readonly mode?: NonNullable<SessionContext["mode"]> | null;
  } = {}
) {
  let clock = 1_000;
  let idle = false;
  const sent: Sent[] = [];
  const wake = new CardWake({
    now: () => clock,
    sendMessage: (message, sendOptions) => {
      sent.push({ message, options: sendOptions });
    },
  });
  const branch: unknown[] = [];
  const timers: (() => void | Promise<void>)[] = [];
  const delays: number[] = [];
  const mode = options.mode === undefined ? "rpc" : options.mode;
  const context: CardWakeContext = {
    ...(options.reportsIdle !== false && { isIdle: () => idle }),
    ...(mode !== null && { mode }),
    setTimeout: (callback, delayMs) => {
      timers.push(callback);
      delays.push(delayMs);
    },
    sessionManager: { getBranch: () => branch },
  };
  let step = 0;
  const card = (label: string, targeted = false): SentCard => {
    const delivery = wake.deliveryFor(targeted, context);
    const fields = {
      customType: "envoy-message",
      content: label,
      display: true,
      details: delivery.details,
    };
    step += 1;
    return {
      delivery,
      message: { role: "custom", ...fields, timestamp: step },
      entry: { type: "custom_message", id: `entry-${label}`, ...fields },
    };
  };
  const message = (record: HostMessage, persisted = true): void => {
    if (persisted) {
      step += 1;
      branch.push(
        record.role === "custom"
          ? { type: "custom_message", id: `entry-${step}`, ...record }
          : { type: "message", id: `entry-${step}`, message: record }
      );
    }
    wake.messageEnd(record, context);
  };
  const reply = (fields: Record<string, unknown> = {}): HostMessage => ({
    role: "assistant",
    content: [{ type: "text", text: "done" }],
    stopReason: "stop",
    ...fields,
  });
  const fire = async (): Promise<void> => {
    for (const timer of timers.splice(0)) await timer();
  };
  return {
    advance: (ms: number) => {
      clock += ms;
    },
    branch,
    card,
    context,
    delays,
    fire,
    message,
    reply,
    sent,
    wake,
    setIdle: (value: boolean) => {
      idle = value;
    },
    fold: (...cards: SentCard[]) => {
      idle = true;
      for (const folded of cards) {
        branch.push(folded.entry);
        wake.messageEnd(folded.message, context);
      }
    },
    endStartup: async (): Promise<void> => {
      const was = idle;
      idle = true;
      const startup = card("startup");
      message(startup.message);
      message(reply());
      await fire();
      idle = was;
    },
    /** A run starts, as the host emits `agent_start`. */
    run: () => wake.agentStart(),
    end: () => wake.agentEnd(context),
  };
}

const wakeMessage = (content: unknown) => ({
  message: {
    content,
    customType: "envoy-message",
    details: LOCAL_ENVOY_NOTICE,
    display: false,
  },
  options: { deliverAs: "nextTurn", triggerTurn: true },
});

const contentOf = (sent: Sent | undefined): string => {
  const message = sent?.message;
  if (typeof message !== "object" || message === null || !("content" in message)) return "";
  return String(message.content);
};

describe("the delivery a card gets", () => {
  test("a card that finds a turn running is an aside that starts a turn", () => {
    const host = harness();
    host.run();

    expect(host.card("busy").delivery).toEqual({
      ...aside,
      details: { envoyCard: expect.any(String), wake: "fold" },
    });
  });

  test("a card that finds the host idle after the startup phase is a steer that starts a turn", async () => {
    const host = harness();
    await host.endStartup();
    host.setIdle(true);

    expect(host.card("idle").delivery).toEqual({
      ...steer,
      details: { envoyCard: expect.any(String), wake: "fold" },
    });
  });

  test("a targeted Aside is an aside whether a turn runs or not", async () => {
    const host = harness();
    await host.endStartup();
    host.run();
    const busy = host.card("busy-aside", true).delivery;
    host.setIdle(true);
    const idle = host.card("idle-aside", true).delivery;

    expect([busy, idle]).toEqual([
      { ...aside, details: { envoyCard: expect.any(String) } },
      { ...aside, details: { envoyCard: expect.any(String) } },
    ]);
  });

  // A context without `isIdle` cannot say no turn runs, and the wake for a folded card needs it,
  // so its cards stay steers with `triggerTurn`; a targeted Aside stays an aside.
  test("a host that reports no idle state gets a steer, and a targeted Aside an aside", () => {
    const host = harness({ reportsIdle: false });

    expect([host.card("card").delivery, host.card("targeted", true).delivery]).toMatchObject([
      steer,
      aside,
    ]);
  });

  test("every card carries an id no other card shares", () => {
    const host = harness();
    const ids = [host.card("one"), host.card("two")].map(
      ({ delivery }) => delivery.details.envoyCard
    );

    expect(new Set(ids).size).toBe(2);
  });
});

// The host folds an aside left over when a run settles into the context with no turn while an
// interrupt's latch is set or plan mode is on: the record's `message_end` reaches the extension and
// the host persists it as a `custom_message` entry, and no run reads it. The wake is one message
// for such cards, decided by the branch's order, because the host does not order a fold's
// `message_end` against the extension's `agent_end`.
describe("a card the host folds into the context with no turn", () => {
  for (const order of ["before", "after"] as const) {
    test(`wakes the session once for a card folded ${order} the run's agent_end`, async () => {
      const host = harness();
      host.run();
      const card = host.card(`folded-${order}`);
      host.message(host.reply());
      if (order === "after") host.end();
      host.fold(card);
      if (order === "before") host.end();
      await host.fire();

      expect(host.sent).toEqual([wakeMessage(expect.stringContaining("Envoy message"))]);
    });
  }

  test("wakes the session once for three cards folded in one window", async () => {
    const host = harness();
    host.run();
    const cards = [host.card("one"), host.card("two"), host.card("three")];
    host.message(host.reply());
    host.end();
    host.fold(...cards);
    await host.fire();

    expect(host.sent).toHaveLength(1);
    expect(contentOf(host.sent[0])).toContain("3 Envoy messages");
  });

  test("counts only the folded card when a card a run read is handled in the same window", async () => {
    const host = harness();
    host.run();
    const read = host.card("read");
    const folded = host.card("folded");
    host.branch.push(read.entry);
    host.message(host.reply());
    host.end();
    host.fold(folded);
    host.wake.messageEnd(read.message, host.context);
    await host.fire();

    expect(host.sent).toHaveLength(1);
    expect(contentOf(host.sent[0])).toContain("1 Envoy message ");
  });

  test("sends no wake for a card the run read, handled after its agent_end", async () => {
    const host = harness();
    host.run();
    const card = host.card("read-mid-run");
    host.branch.push(card.entry);
    host.message(host.reply());
    host.setIdle(true);
    host.end();
    host.wake.messageEnd(card.message, host.context);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("sends no wake for a folded targeted Aside", async () => {
    const host = harness();
    host.run();
    const card = host.card("targeted-aside", true);
    host.fold(card);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("sends no wake for a card folded after a user interrupt", async () => {
    const host = harness();
    host.run();
    const card = host.card("after-interrupt");
    host.message(host.reply(interrupted));
    host.end();
    host.fold(card);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("sends no wake for a card folded after an interrupted auto-compaction", async () => {
    const host = harness();
    host.run();
    const card = host.card("after-compaction");
    host.message(host.reply());
    host.wake.autoCompactionEnd({ aborted: true });
    host.fold(card);
    host.end();
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("sends no wake for a card folded after an interrupt during a retry's backoff", async () => {
    const host = harness();
    host.run();
    const card = host.card("after-retry");
    host.message(host.reply({ stopReason: "error", errorMessage: "overloaded" }));
    host.wake.autoRetryEnd({ success: false, finalError: "Retry cancelled" });
    host.end();
    host.fold(card);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("sends no wake when a run starts between the fold and the check", async () => {
    const host = harness();
    host.run();
    const card = host.card("run-started");
    host.fold(card);
    host.run();
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("sends no wake when the host is busy at the check", async () => {
    const host = harness();
    host.run();
    const card = host.card("busy");
    host.fold(card);
    host.setIdle(false);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  // An Esc while the host sets up a prompt shows to no extension and leaves the host busy at the
  // check with no run: the cards that check found are that interrupt's to keep folded, and a later
  // idle check counts only the cards seen since.
  test("counts only the card seen after a check found the host busy", async () => {
    const host = harness();
    host.run();
    const before = host.card("before-busy-check");
    const after = host.card("after-busy-check");
    host.message(host.reply());
    host.end();
    host.fold(before);
    host.setIdle(false);
    await host.fire();
    host.fold(after);
    await host.fire();

    expect(host.sent).toHaveLength(1);
    expect(contentOf(host.sent[0])).toContain("1 Envoy message ");
  });

  test("sends no wake on a host that reports no idle state", async () => {
    const host = harness({ reportsIdle: false });
    host.run();
    const card = host.card("no-idle");
    host.fold(card);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("sends no wake for the session's own identity-changed notice", async () => {
    const host = harness();
    host.setIdle(true);
    host.message({
      role: "custom",
      customType: "envoy-message",
      content: "session id changed",
      display: true,
      details: LOCAL_ENVOY_NOTICE,
    });
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("sends no wake when the session goes before the check", async () => {
    const host = harness();
    host.run();
    const card = host.card("cancelled");
    host.fold(card);
    host.wake.cancel();
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("wakes the session for a card folded after an abort that was not the user's", async () => {
    const host = harness();
    const aborted = { stopReason: "aborted", errorMessage: "Request was aborted" };
    host.run();
    const card = host.card("other-abort");
    host.message(host.reply(aborted));
    host.end();
    host.fold(card);
    await host.fire();

    expect(host.sent).toHaveLength(1);
  });

  test("wakes the session again once a run starts after an interrupt", async () => {
    const host = harness();
    await host.endStartup();
    host.run();
    host.message(host.reply(interrupted));
    host.end();
    host.setIdle(true);
    // A card reaching the idle session starts a run of its own, which reads it.
    const idleCard = host.card("idle-card");
    host.setIdle(false);
    host.run();
    host.message(idleCard.message);
    await host.fire();
    host.message(host.reply());
    // A card that lands after that run's last step folds with no turn under the latch the
    // interrupt set, which that run did not clear.
    const folded = host.card("folded-later");
    host.fold(folded);
    host.end();
    await host.fire();

    expect(host.sent).toHaveLength(1);
  });
});

// Oh My Pi's RPC mode awaits every send an extension starts during startup, the run an idle card's
// `triggerTurn` starts included, before it reads any command, so a card that reaches an idle host
// in the startup phase starts no turn: it goes as a follow-up, and its own `message_end`, which the
// host emits only after startup released the send, arms the check that wakes the session for it
// once the host is idle.
describe("in the startup phase", () => {
  for (const targeted of [false, true]) {
    test(`sends ${targeted ? "a targeted Aside" : "a card"} that finds an RPC host idle as a follow-up that starts no turn`, () => {
      const host = harness();
      host.setIdle(true);

      expect(host.card("startup-follow-up", targeted).delivery).toEqual({
        ...followUp,
        details: { envoyCard: expect.any(String), wake: "startup" },
      });
    });
  }

  // Print and JSON mode prompt right after their own post-startup awaits, and a turn a wake started
  // there makes that prompt fail; a terminal reads input only once its startup is over. Only RPC
  // mode reads nothing while the host drains the startup sends.
  for (const mode of ["print", "json", "tui", null] as const) {
    test(`sends a card that finds the host idle as a steer in ${mode ?? "a context with no"} mode`, () => {
      const host = harness({ mode });
      host.setIdle(true);

      expect(host.card("startup-steer").delivery).toEqual({
        ...steer,
        details: { envoyCard: expect.any(String), wake: "fold" },
      });
    });
  }

  test("wakes once after the card's message_end with the host idle, and ends the phase", async () => {
    const host = harness();
    host.setIdle(true);
    const card = host.card("startup-wake");
    // A check armed while the card is unseen waits for it, re-armed for the time left.
    host.end();
    await host.fire();
    expect(host.sent).toEqual([]);
    expect(host.delays.at(-1)).toBe(STARTUP_CARD_CAP_MS);

    host.message(card.message);
    await host.fire();
    expect(host.sent).toEqual([
      wakeMessage("1 Envoy message above arrived with no turn to read it: read it and act on it."),
    ]);

    expect(host.card("after-startup").delivery).toMatchObject(steer);
  });

  test("sends one wake for several startup cards", async () => {
    const host = harness();
    host.setIdle(true);
    const cards = [
      host.card("startup-one"),
      host.card("startup-two"),
      host.card("startup-three", true),
    ];
    for (const card of cards) host.message(card.message);
    await host.fire();

    expect(host.sent).toHaveLength(1);
    expect(contentOf(host.sent[0])).toContain("3 Envoy messages");
  });

  test("leaves startup cards a busy check finds to the run, which read them", async () => {
    const host = harness();
    host.setIdle(true);
    const cards = [host.card("busy-read-one"), host.card("busy-read-two")];
    host.setIdle(false);
    host.run();
    for (const card of cards) host.message(card.message);
    await host.fire();
    expect(host.sent).toEqual([]);

    host.message(host.reply());
    host.setIdle(true);
    host.end();
    await host.fire();
    expect(host.sent).toEqual([]);
    // That check found the host idle, which ended the phase.
    expect(host.card("after-busy-read").delivery).toMatchObject(steer);
  });

  test("wakes at the run's end for startup cards a busy check kept and no run read", async () => {
    const host = harness();
    host.setIdle(true);
    const cards = [host.card("busy-unread-one"), host.card("busy-unread-two")];
    host.setIdle(false);
    for (const card of cards) host.message(card.message);
    await host.fire();
    expect(host.sent).toEqual([]);

    host.setIdle(true);
    host.end();
    await host.fire();
    expect(host.sent).toHaveLength(1);
    expect(contentOf(host.sent[0])).toContain("2 Envoy messages");
  });

  test("wakes for a startup card after an interrupted run", async () => {
    const host = harness();
    host.run();
    host.message(host.reply(interrupted));
    host.end();
    host.setIdle(true);
    const card = host.card("startup-after-interrupt");
    expect(card.delivery).toMatchObject(followUp);
    host.message(card.message);
    await host.fire();

    expect(host.sent).toHaveLength(1);
  });

  test("sends a card on a host that reports no idle state as a steer that starts a turn", () => {
    const host = harness({ reportsIdle: false });

    expect(host.card("startup-no-idle").delivery).toMatchObject(steer);
  });

  test("sends a card that finds a turn running as an aside that starts one", () => {
    const host = harness();
    host.run();

    expect(host.card("startup-busy-host").delivery).toMatchObject(aside);
  });

  test("stops waiting for a startup card that never reaches the context after the cap", async () => {
    const host = harness();
    host.setIdle(true);
    // A send the host failed: no message_end ever comes for it.
    host.card("startup-never-seen");
    host.advance(20_000);
    const seen = host.card("startup-seen");
    host.message(seen.message);
    await host.fire();
    expect(host.sent).toEqual([]);
    expect(host.delays.at(-1)).toBe(STARTUP_CARD_CAP_MS - 20_000);

    host.advance(STARTUP_CARD_CAP_MS - 20_000);
    await host.fire();
    expect(host.sent).toHaveLength(1);
    expect(contentOf(host.sent[0])).toContain("1 Envoy message ");
    expect(host.card("after-cap").delivery).toMatchObject(steer);
  });
});

// While the host sets up a prompt (busy, before its run's `agent_start`: the usage preflight,
// `before_agent_start` handlers, pi-envoy's own open-asks read) an interrupt leaves no sign an
// extension sees: no loop runs, so no aborted assistant message comes. The host still folds the
// queued card under its latch, and a wake would start the turn the person stopped.
describe("a card sent while the host sets up a prompt", () => {
  test("gets no wake when the prompt is interrupted before its run starts", async () => {
    const host = harness();
    await host.endStartup();
    const card = host.card("setup");
    expect(card.delivery).toMatchObject(aside);
    host.fold(card);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("gets no wake from the run that then starts", async () => {
    const host = harness();
    await host.endStartup();
    const card = host.card("setup-then-run");
    host.run();
    host.fold(card);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("gets no wake when sent after the run's agent_end and before the host settles", async () => {
    const host = harness();
    await host.endStartup();
    host.run();
    host.message(host.reply());
    host.end();
    const card = host.card("after-agent-end");
    host.fold(card);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  // A switch to a new or resumed session (`agent-session.ts` `newSession` ~10280-10282,
  // `switchSession` ~11906-11907) and a manual or extension compaction (`session-maintenance.ts`
  // ~1201-1202) disconnect the host from the agent before they abort a running run, so that run's
  // `agent_end` never reaches an extension.
  for (const [name, drop] of [
    ["a switch to a new session", (wake) => wake.sessionSwitch({ reason: "new" })],
    ["a switch to a resumed session", (wake) => wake.sessionSwitch({ reason: "resume" })],
    ["a manual compaction", (wake) => wake.sessionCompact()],
  ] as const satisfies readonly (readonly [string, (wake: CardWake) => void])[]) {
    test(`gets no wake when sent after ${name} dropped the run`, async () => {
      const host = harness();
      await host.endStartup();
      host.run();
      drop(host.wake);
      const card = host.card("after-drop");
      host.fold(card);
      await host.fire();

      expect(host.sent).toEqual([]);
    });
  }

  // A host seen idle has no run, whichever event it gave no extension.
  test("gets no wake when sent after a card found the host idle", async () => {
    const host = harness();
    await host.endStartup();
    host.run();
    host.setIdle(true);
    host.card("idle");
    host.setIdle(false);
    const card = host.card("after-idle-card");
    host.fold(card);
    await host.fire();

    expect(host.sent).toEqual([]);
  });

  test("gets no wake when sent after a check found the host idle", async () => {
    const host = harness();
    await host.endStartup();
    host.run();
    host.fold(host.card("woken"));
    await host.fire();
    expect(host.sent).toHaveLength(1);

    host.setIdle(false);
    host.fold(host.card("after-idle-check"));
    await host.fire();

    expect(host.sent).toHaveLength(1);
  });

  // A fork neither disconnects the agent nor aborts the run (`agent-session.ts` `fork`, ~10494),
  // and an automatic compaction runs inside a live run (between steps, or in its post-run
  // maintenance), emitting `session_compact` between its `auto_compaction_start` and
  // `auto_compaction_end`: the run goes on, and its cards keep their wake.
  for (const [name, during] of [
    ["a fork", (wake) => wake.sessionSwitch({ reason: "fork" })],
    [
      "an automatic compaction",
      (wake) => {
        wake.autoCompactionStart();
        wake.sessionCompact();
        wake.autoCompactionEnd({ aborted: false });
      },
    ],
    // A mid-run compaction's `auto_compaction_end` goes to extensions detached, so it can reach
    // this module after the next compaction's start.
    [
      "an automatic compaction that started before the last one's end",
      (wake) => {
        wake.autoCompactionStart();
        wake.autoCompactionStart();
        wake.autoCompactionEnd({ aborted: false });
        wake.sessionCompact();
        wake.autoCompactionEnd({ aborted: false });
      },
    ],
  ] as const satisfies readonly (readonly [string, (wake: CardWake) => void])[]) {
    test(`leaves a card sent in a run after ${name} its wake`, async () => {
      const host = harness();
      await host.endStartup();
      host.run();
      during(host.wake);
      const card = host.card("after-run-goes-on");
      host.message(host.reply());
      host.fold(card);
      host.end();
      await host.fire();

      expect(host.sent).toHaveLength(1);
    });
  }

  // A card that lands in a run's post-run maintenance or auto-compaction (after its last step,
  // before the extension's `agent_end`) is that run's, and a fold under an earlier interrupt's
  // latch still gets its wake.
  test("leaves a card sent in a run's post-run phase its wake", async () => {
    const host = harness();
    await host.endStartup();
    host.run();
    host.message(host.reply());
    const card = host.card("post-run");
    host.fold(card);
    host.end();
    await host.fire();

    expect(host.sent).toHaveLength(1);
  });
});

describe("unreadCards", () => {
  const cardEntry = (id: string) => ({
    type: "custom_message",
    details: { envoyCard: id, wake: "fold" },
  });
  const assistantEntry = { type: "message", message: { role: "assistant" } };

  test("counts a card with no assistant message after it, and one the branch does not hold", () => {
    const branch = [cardEntry("read"), assistantEntry, cardEntry("folded")];

    expect(unreadCards(branch, new Set(["read", "folded", "not-persisted"]))).toBe(2);
  });

  test("leaves the set it was given as it was", () => {
    const cards = new Set(["folded"]);
    unreadCards([cardEntry("folded")], cards);

    expect([...cards]).toEqual(["folded"]);
  });
});

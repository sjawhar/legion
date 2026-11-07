import { describe, expect, test } from "bun:test";
import { DEFAULT_HEARTBEAT_MS, DEFAULT_TMUX_PLACE_CACHE_MS } from "../envoy-settings";
import type { TmuxPlace, TmuxPlaceReading } from "../tmux-place";
import { createTmuxPlaceCache } from "../tmux-place-cache";
import { createEnvoyClient } from "../transport";

const PLACE: TmuxPlace = {
  socket: "/tmp/tmux-1000/default",
  server_pid: "4242",
  session_name: "legion",
  window_id: "@7",
  window_index: 3,
  window_name: "worker",
  pane_id: "%3",
  pane_index: 1,
  layout: "b2f1,208x52,0,0,5",
  zoomed: false,
};

/** The same session after the human moved its pane into a window of its own. */
const MOVED: TmuxPlace = { ...PLACE, window_id: "@9", window_index: 5, window_name: "moved" };

/** How long a reading stands in these cases, the lifetime every `clock` step below is measured
 *  against: the one a session runs on where nothing names another. */
const LIFETIME_MS = DEFAULT_TMUX_PLACE_CACHE_MS;

/** The hops `DRAWING_STALE_AFTER_MS` counts between a pane and the drawing a reader sees
 *  (`packages/dispatch/web/src/features/machines/machine-model.ts`): the registration that
 *  carries the place, which a steady session sends one heartbeat apart, the route roster the
 *  page reads, and the page's own poll. */
const ROUTE_ROSTER_MS = 5_000;
const PAGE_POLL_MS = 15_000;
/** What those hops and the place a registration carries come to — 90 + 5 + 15 + 30 — and the
 *  three minutes the page draws a machine for before it calls the drawing behind. */
const WORST_CASE_DRAWING_MS = DEFAULT_HEARTBEAT_MS + ROUTE_ROSTER_MS + PAGE_POLL_MS + LIFETIME_MS;
const DRAWING_STALE_AFTER_MS = 3 * 60_000;

/** A reading nobody has answered yet, and the count of readings started, so a case can say both
 *  that a reading was asked for and that an ask found one already away. */
function slowReader() {
  const answers: {
    resolve: (reading: TmuxPlaceReading) => void;
    reject: (error: Error) => void;
  }[] = [];
  return {
    answers,
    /** One reading, left unanswered until the case answers it. */
    read: (): Promise<TmuxPlaceReading> => {
      const pending = Promise.withResolvers<TmuxPlaceReading>();
      answers.push(pending);
      return pending.promise;
    },
  };
}

/** Everything a reading's answer touches is written in its own turn, so a case that has answered
 *  one waits for that turn before it reads the cache. */
async function settle(): Promise<void> {
  for (let turn = 0; turn < 5; turn++) await Promise.resolve();
}

/**
 * A clock the case moves by hand, with the cache's refresh timer running on it rather than on
 * the host's: a real 30 seconds per tick would cost every run two minutes. `advanceTo` carries
 * the clock to a time and fires every refresh due strictly before it, letting each reading land,
 * so a refresh due at the very instant of an ask is taken as landing after that ask — the worst
 * order for the place the ask carries, and the one a drifting host timer gives.
 */
function fakeClock() {
  let nowMs = 0;
  const timers: { everyMs: number; dueAt: number; run: () => void; stopped: boolean }[] = [];
  return {
    timers,
    now: (): number => nowMs,
    every: (everyMs: number, run: () => void): (() => void) => {
      const timer = { everyMs, dueAt: nowMs + everyMs, run, stopped: false };
      timers.push(timer);
      return () => {
        timer.stopped = true;
      };
    },
    /** The timers still running, so a case can say the shutdown stopped the refresh. */
    live: () => timers.filter((timer) => !timer.stopped),
    advanceTo: async (toMs: number): Promise<void> => {
      for (;;) {
        const due = timers
          .filter((timer) => !timer.stopped && timer.dueAt < toMs)
          .sort((one, other) => one.dueAt - other.dueAt)[0];
        if (due === undefined) break;
        nowMs = due.dueAt;
        due.dueAt += due.everyMs;
        due.run();
        await settle();
      }
      nowMs = toMs;
    },
  };
}

/** A reading that answers at once with the place tmux holds at the clock's time, stamped with
 *  that time, so a case can say how old the place a registration carries is. */
function stampedReader(clock: { now: () => number }) {
  const startedAt: number[] = [];
  return {
    startedAt,
    read: async (): Promise<TmuxPlaceReading> => {
      const at = clock.now();
      startedAt.push(at);
      return { kind: "place", place: { ...PLACE, window_name: `read-at-${at}` } };
    },
  };
}

/** How old the place an ask carried is: the clock at the ask, less the clock the reading behind
 *  that place was stamped with. */
function ageOf(place: TmuxPlace | undefined, atMs: number): number | undefined {
  if (place === undefined) return undefined;
  return atMs - Number(place.window_name.replace("read-at-", ""));
}

describe("createTmuxPlaceCache", () => {
  test("answers at once and starts the reading the next ask is answered from", async () => {
    const reader = slowReader();
    let clock = 1_000;
    const cache = createTmuxPlaceCache({
      read: reader.read,
      lifetimeMs: () => LIFETIME_MS,
      now: () => clock,
    });

    // The first ask has no reading behind it: it answers none rather than waiting for one.
    expect(cache.place()).toBeUndefined();
    expect(reader.answers).toHaveLength(1);
    reader.answers[0]?.resolve({ kind: "place", place: PLACE });
    await settle();

    expect(cache.place()).toEqual(PLACE);
    // Inside the lifetime the reading stands, however many asks arrive.
    clock += LIFETIME_MS - 1;
    expect(cache.place()).toEqual(PLACE);
    expect(reader.answers).toHaveLength(1);
    // Past it the ask still answers at once, with the place it holds, and starts the reading the
    // ask after it is answered from.
    clock += 1;
    expect(cache.place()).toEqual(PLACE);
    expect(reader.answers).toHaveLength(2);
    reader.answers[1]?.resolve({ kind: "place", place: MOVED });
    await settle();

    expect(cache.place()).toEqual(MOVED);
  });

  test("one reading serves every ask that arrives while it is away", async () => {
    const reader = slowReader();
    const cache = createTmuxPlaceCache({
      read: reader.read,
      lifetimeMs: () => LIFETIME_MS,
      now: () => 1_000,
    });

    // Two registrations in flight together — a queued pass and the heartbeat's stall backstop —
    // must not each shell out to tmux: the slower of the two would overwrite the newer answer.
    cache.place();
    cache.place();

    expect(reader.answers).toHaveLength(1);
  });

  test("a refused reading clears the place it held", async () => {
    const reader = slowReader();
    let clock = 1_000;
    const cache = createTmuxPlaceCache({
      read: reader.read,
      lifetimeMs: () => LIFETIME_MS,
      now: () => clock,
    });
    cache.place();
    reader.answers[0]?.resolve({ kind: "place", place: PLACE });
    await settle();

    // The session left its pane: the listener's entry must lose the place, not keep a stale one.
    clock += LIFETIME_MS;
    cache.place();
    reader.answers[1]?.resolve({ kind: "refused" });
    await settle();

    expect(cache.place()).toBeUndefined();
  });

  test("a timed-out reading keeps the place it held, and costs one call per lifetime", async () => {
    const reader = slowReader();
    let clock = 1_000;
    const cache = createTmuxPlaceCache({
      read: reader.read,
      lifetimeMs: () => LIFETIME_MS,
      now: () => clock,
    });
    cache.place();
    reader.answers[0]?.resolve({ kind: "place", place: PLACE });
    await settle();

    // A server too loaded to answer in time must not take a live session off the layout, and it
    // restamps the lifetime all the same: a loaded server is asked once a lifetime, not once a
    // registration.
    clock += LIFETIME_MS;
    cache.place();
    reader.answers[1]?.resolve({ kind: "timedOut" });
    await settle();

    expect(cache.place()).toEqual(PLACE);
    expect(reader.answers).toHaveLength(2);
  });

  test("a reading that throws reaches the failure hook alone", async () => {
    const reader = slowReader();
    let clock = 1_000;
    const failures: unknown[] = [];
    const cache = createTmuxPlaceCache({
      read: reader.read,
      lifetimeMs: () => LIFETIME_MS,
      now: () => clock,
      onFailure: (error) => failures.push(error),
    });
    cache.place();
    reader.answers[0]?.resolve({ kind: "place", place: PLACE });
    await settle();

    clock += LIFETIME_MS;
    cache.place();
    reader.answers[1]?.reject(new Error("the tmux reader broke"));
    await settle();

    // A reader that throws says nothing about where the session sits, so the place stands as it
    // does for a timeout; the failure is reported and reaches no ask.
    expect(cache.place()).toEqual(PLACE);
    expect(failures.map((error) => (error as Error).message)).toEqual(["the tmux reader broke"]);
    expect(reader.answers).toHaveLength(2);
  });

  // The registration path itself: the place is read in the turn the body is built, so no tmux
  // call sits between a registration's start and its POST. A reading that answers after the
  // session's shutdown has been seen writes no place and starts none, so nothing can reach the
  // listener behind the DELETE and write the entry again for a session nobody heartbeats
  // (dispatch://INFRA-392).
  test("a slow reading delays no registration and writes nothing once the session has gone", async () => {
    const reader = slowReader();
    let clock = 1_000;
    const cache = createTmuxPlaceCache({
      read: reader.read,
      lifetimeMs: () => LIFETIME_MS,
      now: () => clock,
    });
    const wire: string[] = [];
    const bodies: Record<string, unknown>[] = [];
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async (input, init) => {
        const url = new URL(input.toString());
        wire.push(`${init?.method ?? "GET"} ${url.pathname}`);
        if (url.pathname !== "/v1/interests/subscribe") return Response.json({});
        const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
        bodies.push(body);
        return Response.json({
          session_id: body.session_id,
          machine_id: "host-a",
          dir: body.dir,
          topics: body.topics,
        });
      },
    });
    /** One registration, built and posted the way a session's heartbeat builds and posts it. */
    const register = async (): Promise<void> => {
      const place = cache.place();
      await client.subscribe({
        sessionID: "ses_slow",
        directory: "/work",
        topics: ["notifications.agent.ses_slow"],
        port: 0,
        title: "Envoy QA",
        driving: false,
        plugin: { name: "@sjawhar/pi-envoy", version: "11.0.0" },
        ...(place === undefined ? {} : { place }),
      });
    };
    // The registration goes out in the turn it is built — the stand-in listener records it from
    // inside `subscribe` — with the reading still away: it carries no place rather than waiting
    // out a cold `tmux display-message` of up to a second.
    const first = register();
    expect(reader.answers).toHaveLength(1);
    expect(wire).toEqual(["POST /v1/interests/subscribe"]);
    expect(bodies[0]?.tmux_place).toBeUndefined();
    await first;

    // The reading lands between registrations, and the next one carries it.
    reader.answers[0]?.resolve({ kind: "place", place: PLACE });
    await settle();
    await register();
    expect(bodies.at(-1)?.tmux_place).toEqual(PLACE);

    // Past the lifetime a registration carries the place it holds and starts the reading that
    // this session, as it turns out, does not live to send. This is the one the race is about:
    // it reaches the listener now, before the shutdown, and not behind the DELETE.
    clock += LIFETIME_MS;
    const last = register();
    expect(reader.answers).toHaveLength(2);
    expect(bodies.at(-1)?.tmux_place).toEqual(PLACE);

    // The session shuts down and its entry is deleted while that reading is still away.
    cache.shutDown();
    await client.unregisterSession({ sessionID: "ses_slow" });
    reader.answers[1]?.resolve({ kind: "place", place: MOVED });
    await settle();
    await last;

    // The late answer wrote no place and started no further reading, and the wire ends at the
    // DELETE: nothing this session reported outlives it at the listener, so no peer sees it
    // live and then unresponsive for the gone horizon.
    expect(cache.place()).toEqual(PLACE);
    expect(reader.answers).toHaveLength(2);
    expect(wire).toEqual([
      "POST /v1/interests/subscribe",
      "POST /v1/interests/subscribe",
      "POST /v1/interests/subscribe",
      "DELETE /v1/sessions/ses_slow",
    ]);
  });

  // The steady state, which is heartbeats and nothing else: an ask every `DEFAULT_HEARTBEAT_MS`
  // against a lifetime a third as long. Were `place` the only thing that started a reading, every
  // heartbeat would carry the reading the heartbeat before it started — a place 90 s old, and a
  // pane that moved taking two heartbeats to show — which is past what `DRAWING_STALE_AFTER_MS`
  // allows the hops between a pane and the drawing. The cache refreshes itself between the asks
  // instead, on a timer of its own at the lifetime.
  test("refreshes itself between heartbeats, so a drawing stays inside the stale bound", async () => {
    const clock = fakeClock();
    const reader = stampedReader(clock);
    const cache = createTmuxPlaceCache({
      read: reader.read,
      lifetimeMs: () => LIFETIME_MS,
      now: clock.now,
      every: clock.every,
    });

    const asks = [0, DEFAULT_HEARTBEAT_MS, 2 * DEFAULT_HEARTBEAT_MS, 3 * DEFAULT_HEARTBEAT_MS];
    const carriedAgesMs: (number | undefined)[] = [];
    for (const at of asks) {
      await clock.advanceTo(at);
      carriedAgesMs.push(ageOf(cache.place(), at));
      await settle();
    }

    // The first ask carries nothing — it is the one that starts the first reading, and waits on
    // no tmux call — and every ask after it carries a place one lifetime old, not one heartbeat.
    // That is what the three minutes rests on: 90 s of registration, 5 s of roster, 15 s of poll
    // and the lifetime, so a drawing a reader is shown as fresh is inside that of tmux.
    const oldestCarriedMs = Math.max(...carriedAgesMs.map((age) => age ?? 0));
    const worstCaseMs = DEFAULT_HEARTBEAT_MS + ROUTE_ROSTER_MS + PAGE_POLL_MS + oldestCarriedMs;
    expect({ carriedAgesMs, worstCaseMs }).toEqual({
      carriedAgesMs: [undefined, LIFETIME_MS, LIFETIME_MS, LIFETIME_MS],
      worstCaseMs: WORST_CASE_DRAWING_MS,
    });
    expect(worstCaseMs).toBeLessThan(DRAWING_STALE_AFTER_MS);

    // One timer for the life of the cache, armed by the first ask at the lifetime it read: the
    // asks after it arm none of their own, and a cache nobody asked would have armed none at all.
    expect(clock.timers).toHaveLength(1);
    expect(clock.timers[0]?.everyMs).toBe(LIFETIME_MS);
    // The registration path gains nothing from any of it: the ask still answers with the place
    // itself, in the turn it is made, which is what a registration body is built from.
    expect(cache.place()).toEqual({ ...PLACE, window_name: `read-at-${3 * DEFAULT_HEARTBEAT_MS}` });

    // The shutdown stops the refresh: no timer of this session's outlives it, and no reading
    // starts once it has been seen, however long the host runs on.
    const readingsAtShutdown = reader.startedAt.length;
    cache.shutDown();
    expect(clock.live()).toEqual([]);
    await clock.advanceTo(3 * DEFAULT_HEARTBEAT_MS + 10 * LIFETIME_MS);

    // And a session whose readings have stopped is a session the page calls behind: with nothing
    // refreshing the place, its age passes the three minutes rather than sitting fresh forever.
    expect(reader.startedAt).toHaveLength(readingsAtShutdown);
    expect(ageOf(cache.place(), clock.now())).toBeGreaterThan(DRAWING_STALE_AFTER_MS);
  });
});

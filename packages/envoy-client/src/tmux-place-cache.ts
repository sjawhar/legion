// The place a registration carries, held between readings so that no registration ever waits on
// tmux. A cold reading is a `tmux display-message` of up to `TMUX_PANE_LOOKUP_MS`, and a
// registration that waited one out could reach the listener after the DELETE its own session's
// shutdown sent: the listener would write the entry again, with nobody left to heartbeat it, and
// peers would read the session as live and then unresponsive for the gone horizon
// (dispatch://INFRA-392). So a reading runs between registrations and never inside one. A
// registration takes whatever this holds at the moment it is built — the place when a reading has
// landed, none when none has — and a session whose shutdown has been seen stops every reading,
// the one already away included, from writing anything at all.
//
// Between registrations the cache keeps itself current on a timer of its own, because the asks
// are further apart than the lifetime: in steady state the only ask is the heartbeat, one every
// `SESSION_HEARTBEAT_MS`, three times the default lifetime. An ask-driven reading alone would
// hand every heartbeat the reading the heartbeat before it started, so each registration would
// carry a place a whole heartbeat old and a split would take two heartbeats to show on
// `/machines`, past what `DRAWING_STALE_AFTER_MS` allows for the hops.

import type { TmuxPlace, TmuxPlaceReading } from "./tmux-place";

/** The seams one cache runs on: what a reading is, how long one stands, the clock the lifetime is
 *  measured on, the timer the refresh runs on, and where a reading that threw is reported. */
export type TmuxPlaceCacheOptions = {
  /** One reading of where the session sits (`readTmuxPlace`). Never run twice over: a reading
   *  already away serves every ask that arrives while it runs. */
  readonly read: () => Promise<TmuxPlaceReading>;
  /** How long one reading stands, asked at every refresh rather than once, so a session reading
   *  its settings late (`tmuxPlaceCacheMs`) is not held to the value they had at start-up. */
  readonly lifetimeMs: () => number;
  /** The clock the lifetime is measured on; `Date.now` where none is given. */
  readonly now?: () => number;
  /** Runs `callback` every `ms` until the returned stop runs; an unreferenced `setInterval` by
   *  default, so the refresh of a cache nobody has shut down holds no host process open. A case
   *  gives its own to drive the refresh on the clock it drives `now` on. */
  readonly every?: (ms: number, callback: () => void) => () => void;
  /** Told of a reading that threw, which is the only trace one leaves: a failed reading must
   *  never fail the registration that asked, and with it the heartbeat. */
  readonly onFailure?: (error: unknown) => void;
};

export type TmuxPlaceCache = {
  /**
   * Where the session sits, as the last reading left it, with no wait of any kind: the caller
   * builds its registration body from this and posts it in the same turn. The first ask starts
   * the refresh timer, and starts the reading the ask after it is answered from; from then on
   * the timer keeps the held reading inside its lifetime, so every later ask carries a place
   * that describes tmux as of at most one lifetime and one reading ago, however far apart the
   * asks are. The ask itself still starts a reading when the held one has outlived its lifetime,
   * so a cache nobody asks runs no call and a first ask is never left with nothing coming.
   */
  readonly place: () => TmuxPlace | undefined;
  /**
   * Says the session is shutting down. From here on the refresh timer is stopped and no reading
   * starts, and the one already away writes no place and stamps no lifetime when it lands, so
   * nothing a slow tmux answers can reach a registration the shutdown's DELETE has already
   * overtaken.
   */
  readonly shutDown: () => void;
};

export function createTmuxPlaceCache(options: TmuxPlaceCacheOptions): TmuxPlaceCache {
  const now = options.now ?? Date.now;
  const repeat = options.every ?? every;
  let held: TmuxPlace | undefined;
  let readAt: number | undefined;
  let reading = false;
  let shuttingDown = false;
  let stopRefreshing: (() => void) | undefined;

  // One reading, which the next ask is answered from. `refused` drops the place, which clears the
  // listener's entry for a session that has left its pane; `timedOut` keeps the last place read,
  // so one slow call does not take a live session off the layout; a reader that throws says
  // nothing about where the session sits, so it is kept like a timeout. A reading of any kind
  // restamps the lifetime, so a session outside tmux and a server too loaded to answer each cost
  // one call per lifetime rather than one per registration.
  const startReading = (): void => {
    if (reading || shuttingDown) return;
    reading = true;
    void (async () => {
      try {
        const answer = await options.read();
        if (shuttingDown) return;
        if (answer.kind === "place") held = answer.place;
        else if (answer.kind === "refused") held = undefined;
        readAt = now();
      } catch (error) {
        if (shuttingDown) return;
        options.onFailure?.(error);
        readAt = now();
      } finally {
        reading = false;
      }
      // Nothing awaits this reading, so a hook that throws would reach the host as an unhandled
      // rejection, which Oh My Pi treats as fatal; a reading may cost the session nothing.
    })().catch(() => undefined);
  };

  // The refresh timer, armed by the first ask at the lifetime that ask reads: a cache nobody has
  // asked reads no tmux at all, which is what keeps a session outside any pane from polling a
  // server it has no place on. Its tick reads outright rather than through the lifetime gate
  // below, because a reading is stamped when it lands: a tick one lifetime after that stamp is
  // always a hair short of the lifetime, and gating it would halve the refreshes and leave the
  // held place up to two lifetimes old.
  const keepFresh = (): void => {
    if (stopRefreshing !== undefined || shuttingDown) return;
    stopRefreshing = repeat(options.lifetimeMs(), startReading);
  };

  return {
    place: () => {
      keepFresh();
      if (readAt === undefined || now() - readAt >= options.lifetimeMs()) startReading();
      return held;
    },
    shutDown: () => {
      shuttingDown = true;
      stopRefreshing?.();
      stopRefreshing = undefined;
    },
  };
}

function every(ms: number, callback: () => void): () => void {
  const timer = setInterval(callback, ms);
  // A session's own refresh must never be what holds its process alive: a host that has nothing
  // left to do exits, and the cache's last tick goes with it.
  timer.unref?.();
  return () => clearInterval(timer);
}

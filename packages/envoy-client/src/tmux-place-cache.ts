// The place a registration carries, held between readings so that no registration ever waits on
// tmux. A cold reading is a `tmux display-message` of up to `TMUX_PANE_LOOKUP_MS`, and a
// registration that waited one out could reach the listener after the DELETE its own session's
// shutdown sent: the listener would write the entry again, with nobody left to heartbeat it, and
// peers would read the session as live and then unresponsive for the gone horizon
// (dispatch://INFRA-392). So a reading runs between registrations and never inside one. A
// registration takes whatever this holds at the moment it is built — the place when a reading has
// landed, none when none has — and a session whose shutdown has been seen stops every reading,
// the one already away included, from writing anything at all.

import type { TmuxPlace, TmuxPlaceReading } from "./tmux-place";

/** The seams one cache runs on: what a reading is, how long one stands, the clock the lifetime is
 *  measured on, and where a reading that threw is reported. */
export type TmuxPlaceCacheOptions = {
  /** One reading of where the session sits (`readTmuxPlace`). Never run twice over: a reading
   *  already away serves every ask that arrives while it runs. */
  readonly read: () => Promise<TmuxPlaceReading>;
  /** How long one reading stands, asked at every refresh rather than once, so a session reading
   *  its settings late (`tmuxPlaceCacheMs`) is not held to the value they had at start-up. */
  readonly lifetimeMs: () => number;
  /** The clock the lifetime is measured on; `Date.now` where none is given. */
  readonly now?: () => number;
  /** Told of a reading that threw, which is the only trace one leaves: a failed reading must
   *  never fail the registration that asked, and with it the heartbeat. */
  readonly onFailure?: (error: unknown) => void;
};

export type TmuxPlaceCache = {
  /**
   * Where the session sits, as the last reading left it, with no wait of any kind: the caller
   * builds its registration body from this and posts it in the same turn. The ask also starts
   * the next reading when the held one has outlived its lifetime, or when there has been none,
   * so the place a registration carries is at most one lifetime old and the reading is paid for
   * between registrations rather than inside one.
   */
  readonly place: () => TmuxPlace | undefined;
  /**
   * Says the session is shutting down. From here on no reading starts, and the one already away
   * writes no place and stamps no lifetime when it lands, so nothing a slow tmux answers can
   * reach a registration the shutdown's DELETE has already overtaken.
   */
  readonly shutDown: () => void;
};

export function createTmuxPlaceCache(options: TmuxPlaceCacheOptions): TmuxPlaceCache {
  const now = options.now ?? Date.now;
  let held: TmuxPlace | undefined;
  let readAt: number | undefined;
  let reading = false;
  let shuttingDown = false;

  // The reading the next ask is answered from, where one is owed. `refused` drops the place,
  // which clears the listener's entry for a session that has left its pane; `timedOut` keeps the
  // last place read, so one slow call does not take a live session off the layout; a reader that
  // throws says nothing about where the session sits, so it is kept like a timeout. A reading of
  // any kind restamps the lifetime, so a session outside tmux and a server too loaded to answer
  // each cost one call per lifetime rather than one per registration.
  const refresh = (): void => {
    if (reading || shuttingDown) return;
    if (readAt !== undefined && now() - readAt < options.lifetimeMs()) return;
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

  return {
    place: () => {
      refresh();
      return held;
    },
    shutDown: () => {
      shuttingDown = true;
    },
  };
}

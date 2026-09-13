/**
 * The bound on `ProcessManager.handleException`'s re-send of a role-lane message whose delivery
 * to an alive root architect failed (`delivery_failed`, `no_holder`). Before this ledger every
 * such exception re-sent the message at once and unbounded: a holder busy for a minute turned one
 * message into dozens of paid model turns (LEGION-101). Constants, not configuration — nobody
 * tunes them.
 *
 * - `RESEND_PAUSES_MS[k - 1]` is the pause before re-send `k`, including the first: with an older
 *   listener a `delivery_failed` on an alive holder is almost always a busy holder, and waiting
 *   5 s before the first copy lets its turn end. 5 s, 15 s, 45 s: a holder busy for about a
 *   minute sees at most three copies, the last one after it is likely idle.
 * - `MAX_RESENDS` re-sends per message, then one cap log line and the entry is dropped; the resync
 *   backstop and the role's next catch-up recover the holder.
 * - `RESEND_LEDGER_TTL_MS`: an entry no exception has touched for this long is forgotten. The
 *   exception for a re-sent copy arrives within seconds of the re-send (the listener's 2 s receipt
 *   window), so five minutes of silence means the chain ended in delivery. Pruned on every
 *   `claim`, so the map is bounded without a timer.
 */
export const RESEND_PAUSES_MS = [5_000, 15_000, 45_000] as const;
export const MAX_RESENDS = RESEND_PAUSES_MS.length;
export const RESEND_LEDGER_TTL_MS = 5 * 60_000;

export type ResendDecision =
  | { kind: "resend"; attempt: number; pauseMs: number }
  | { kind: "in-flight" }
  | { kind: "capped"; attempts: number };

interface ResendEntry {
  attempts: number;
  inFlight: boolean;
  touchedAt: number;
}

/**
 * In memory only, on the `ProcessManager` instance: a daemon restart forgets every chain, and
 * also ends every one (nothing is in flight afterwards to fail again). Keyed by the caller — the
 * message itself, `original.topic + "\n" + original.payload` (`resendChainKey` in
 * `processes.ts`), never the event id: the listener mints a fresh `event_id` for every publish
 * and the exception reports the failed copy's, so an event id identifies one copy, never the
 * chain. The ledger stores nothing beyond the key string and the three fields above, and never
 * logs a payload.
 */
export class ResendLedger {
  private readonly entries = new Map<string, ResendEntry>();

  constructor(private readonly now: () => number) {}

  /**
   * Asks whether the exception that just arrived for `key` may re-send. `resend` names the attempt
   * (1-based) and the pause that must precede it; the entry is marked in flight until `settle`.
   * `in-flight` means a re-send for this message is still pending its pause: the exception is a
   * duplicate of the one that started it (two near-simultaneous failures of one copy), not the
   * failure of the pending copy, and is not counted — only the re-send in flight can produce the
   * chain's next legitimate exception. `capped` means `MAX_RESENDS` re-sends have already gone
   * out: the entry is dropped so the same message, failing again much later, starts a fresh
   * chain rather than being silenced forever.
   */
  claim(key: string): ResendDecision {
    const now = this.now();
    for (const [entryKey, entry] of this.entries) {
      if (now - entry.touchedAt > RESEND_LEDGER_TTL_MS) this.entries.delete(entryKey);
    }
    const entry = this.entries.get(key);
    if (!entry) {
      this.entries.set(key, { attempts: 1, inFlight: true, touchedAt: now });
      return { kind: "resend", attempt: 1, pauseMs: RESEND_PAUSES_MS[0] };
    }
    entry.touchedAt = now;
    if (entry.inFlight) return { kind: "in-flight" };
    if (entry.attempts >= MAX_RESENDS) {
      this.entries.delete(key);
      return { kind: "capped", attempts: entry.attempts };
    }
    entry.attempts += 1;
    entry.inFlight = true;
    // `attempts` is within 1..MAX_RESENDS here, so the index is always a defined pause.
    return {
      kind: "resend",
      attempt: entry.attempts,
      pauseMs: RESEND_PAUSES_MS[entry.attempts - 1] as number,
    };
  }

  /** Ends the in-flight mark once the re-send has fired — or the chain was abandoned because the
   * process was dead or the daemon disposed — so the next exception for `key` is judged as the
   * failure of that copy rather than misread as still pending. A no-op for a forgotten key. */
  settle(key: string): void {
    const entry = this.entries.get(key);
    if (!entry) return;
    entry.inFlight = false;
    entry.touchedAt = this.now();
  }

  /** Forgets every chain. Called from `ProcessManager.dispose()`. */
  clear(): void {
    this.entries.clear();
  }
}

import { describe, expect, it } from "bun:test";
import {
  MAX_RESENDS,
  RESEND_LEDGER_TTL_MS,
  RESEND_PAUSES_MS,
  ResendLedger,
} from "../resend-ledger";

const T0 = Date.parse("2026-08-24T00:00:00.000Z");

describe("ResendLedger", () => {
  it("claims three attempts with pauses 5 s, 15 s, 45 s, then reports capped and keeps the entry capped until the TTL elapses", () => {
    let now = T0;
    const ledger = new ResendLedger(() => now);
    const key = "notifications.role.x\n{}";

    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 1, pauseMs: 5_000 });
    ledger.settle(key);
    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 2, pauseMs: 15_000 });
    ledger.settle(key);
    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 3, pauseMs: 45_000 });
    ledger.settle(key);

    expect(MAX_RESENDS).toBe(3);
    expect(ledger.claim(key)).toEqual({ kind: "capped", attempts: 3, justCapped: true });
    // The cap keeps the entry: a fifth and sixth failure inside the window are capped too — the
    // same message never restarts at attempt 1 one second after its cap line (production,
    // 2026-09-15: 83 re-sends against 11 cap lines when the cap dropped the entry).
    expect(ledger.claim(key)).toEqual({ kind: "capped", attempts: 3, justCapped: false });
    now = T0 + RESEND_LEDGER_TTL_MS;
    expect(ledger.claim(key)).toEqual({ kind: "capped", attempts: 3, justCapped: false });
    // Only a full quiet window forgets it; then the same message starts a fresh chain.
    now = T0 + RESEND_LEDGER_TTL_MS + RESEND_LEDGER_TTL_MS + 1;
    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 1, pauseMs: RESEND_PAUSES_MS[0] });
  });

  it("a capped claim refreshes the TTL clock, so a message that keeps failing stays silent instead of restarting", () => {
    let now = T0;
    const ledger = new ResendLedger(() => now);
    const key = "notifications.role.x\n{}";
    for (let attempt = 1; attempt <= MAX_RESENDS; attempt += 1) {
      ledger.claim(key);
      ledger.settle(key);
    }
    expect(ledger.claim(key)).toEqual({ kind: "capped", attempts: 3, justCapped: true });

    // Failures every half-window for two full windows: each capped claim is touched, so the
    // entry never goes a whole TTL without a touch and never expires.
    for (let step = 1; step <= 4; step += 1) {
      now = T0 + step * (RESEND_LEDGER_TTL_MS / 2);
      expect(ledger.claim(key)).toEqual({ kind: "capped", attempts: 3, justCapped: false });
    }
    // A whole quiet window after the last touch, and the message starts over.
    now += RESEND_LEDGER_TTL_MS + 1;
    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 1, pauseMs: 5_000 });
  });

  it("reports in-flight without counting until settle", () => {
    const ledger = new ResendLedger(() => T0);
    const key = "notifications.role.x\n{}";

    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 1, pauseMs: 5_000 });
    expect(ledger.claim(key)).toEqual({ kind: "in-flight" });
    expect(ledger.claim(key)).toEqual({ kind: "in-flight" });
    ledger.settle(key);
    // The two in-flight claims were not counted: this is attempt 2, not 4.
    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 2, pauseMs: 15_000 });
  });

  it("forgets an entry idle past RESEND_LEDGER_TTL_MS but keeps a fresh one", () => {
    let now = T0;
    const ledger = new ResendLedger(() => now);
    const a = "notifications.role.a\n{}";
    const b = "notifications.role.b\n{}";

    ledger.claim(a);
    ledger.settle(a);
    now = T0 + RESEND_LEDGER_TTL_MS / 2;
    ledger.claim(b);
    ledger.settle(b);

    now = T0 + RESEND_LEDGER_TTL_MS + 1;
    expect(ledger.claim(a)).toEqual({ kind: "resend", attempt: 1, pauseMs: 5_000 });
    expect(ledger.claim(b)).toEqual({ kind: "resend", attempt: 2, pauseMs: 15_000 });
  });

  it("keeps independent counts per key", () => {
    const ledger = new ResendLedger(() => T0);
    const a = "notifications.role.a\n{}";
    const b = 'notifications.role.a\n{"other":true}';

    expect(ledger.claim(a)).toEqual({ kind: "resend", attempt: 1, pauseMs: 5_000 });
    expect(ledger.claim(b)).toEqual({ kind: "resend", attempt: 1, pauseMs: 5_000 });
    ledger.settle(a);
    ledger.settle(b);
    expect(ledger.claim(a)).toEqual({ kind: "resend", attempt: 2, pauseMs: 15_000 });
    expect(ledger.claim(b)).toEqual({ kind: "resend", attempt: 2, pauseMs: 15_000 });
  });

  it("clear() empties the ledger so every key starts over", () => {
    const ledger = new ResendLedger(() => T0);
    const key = "notifications.role.x\n{}";
    ledger.claim(key);
    ledger.settle(key);
    ledger.claim(key);

    ledger.clear();

    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 1, pauseMs: 5_000 });
  });
});

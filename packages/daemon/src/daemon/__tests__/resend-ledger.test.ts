import { describe, expect, it } from "bun:test";
import {
  MAX_RESENDS,
  RESEND_LEDGER_TTL_MS,
  RESEND_PAUSES_MS,
  ResendLedger,
} from "../resend-ledger";

const T0 = Date.parse("2026-08-24T00:00:00.000Z");

describe("ResendLedger", () => {
  it("claims three attempts with pauses 5 s, 15 s, 45 s, then reports capped and drops the entry", () => {
    const ledger = new ResendLedger(() => T0);
    const key = "notifications.role.x\n{}";

    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 1, pauseMs: 5_000 });
    ledger.settle(key);
    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 2, pauseMs: 15_000 });
    ledger.settle(key);
    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 3, pauseMs: 45_000 });
    ledger.settle(key);

    expect(MAX_RESENDS).toBe(3);
    expect(ledger.claim(key)).toEqual({ kind: "capped", attempts: 3 });
    // The cap dropped the entry: the same message starts a fresh chain at once.
    expect(ledger.claim(key)).toEqual({ kind: "resend", attempt: 1, pauseMs: RESEND_PAUSES_MS[0] });
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

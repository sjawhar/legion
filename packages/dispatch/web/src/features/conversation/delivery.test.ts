import { expect, test } from "bun:test";
import { DELIVERY_DUPLICATE_WINDOW_MS, RECEIPT_TIMEOUT_CAUSE } from "@legion/contracts";

import { type DeliveryOutcome, isSafeRetry, safeRetryGuidance, withGuidance } from "./delivery";

const now = Date.parse("2026-09-25T12:00:00Z");

function attempt(overrides: Partial<DeliveryOutcome> = {}): DeliveryOutcome {
  return { state: "failed", createdAt: new Date(now - 60_000).toISOString(), ...overrides };
}

// LEGION-271. The promise "Retry won't deliver it twice" is only true while the stream still
// recognises the repeat. Past its duplicate window the retry publishes a second frame — the
// defect this issue exists to close, reached by acting on a promise that outlived its truth.
test("a fresh failed attempt can still be promised a safe retry", () => {
  expect(isSafeRetry(attempt(), now)).toBe(true);
});

test("a failed attempt older than the duplicate window cannot", () => {
  const stale = attempt({
    createdAt: new Date(now - DELIVERY_DUPLICATE_WINDOW_MS - 1000).toISOString(),
  });
  expect(isSafeRetry(stale, now)).toBe(false);
});

test("the boundary itself is outside the promise", () => {
  const exactly = attempt({
    createdAt: new Date(now - DELIVERY_DUPLICATE_WINDOW_MS).toISOString(),
  });
  expect(isSafeRetry(exactly, now)).toBe(false);
  const justInside = attempt({
    createdAt: new Date(now - DELIVERY_DUPLICATE_WINDOW_MS + 1000).toISOString(),
  });
  expect(isSafeRetry(justInside, now)).toBe(true);
});

test("a delivered attempt has no failure to retry", () => {
  expect(isSafeRetry(attempt({ state: "sent" }), now)).toBe(false);
  expect(isSafeRetry(attempt({ state: "pending" }), now)).toBe(false);
});

test("an attempt the stream already had is not a safe retry either", () => {
  expect(isSafeRetry(attempt({ state: "sent", duplicate: true }), now)).toBe(false);
  expect(isSafeRetry(attempt({ duplicate: true }), now)).toBe(false);
});

// The browser's clock is not the server's. `createdAt` is Postgres-stamped and `now` defaults to
// Date.now(), so a client running behind the server sees a NEGATIVE age on an attempt that just
// failed. Since this predicate gates whether the same-mode Retry renders at all, treating that
// as "outside the window" hides the safe action on the primary case - and leaves the
// mode-change buttons, which are a genuine second delivery, as the only thing on offer.
test("a client whose clock trails the server still gets the safe retry", () => {
  const stamped = new Date(now).toISOString();
  for (const skew of [2_000, 30_000, 300_000]) {
    expect(isSafeRetry(attempt({ createdAt: stamped }), now - skew)).toBe(true);
  }
});

test("a createdAt after now is the youngest attempt there is, so it is safest", () => {
  const future = attempt({ createdAt: new Date(now + 1000).toISOString() });
  expect(isSafeRetry(future, now)).toBe(true);
});

test("an unparseable timestamp is not promised anything", () => {
  expect(isSafeRetry(attempt({ createdAt: "not a date" }), now)).toBe(false);
});

// Only the receipt-timeout cause ends in a sentence, so joining with a bare space ran every
// other cause into the guidance: "Failed: no live session Retry won't deliver it twice."
test("the guidance is separated from a cause that has no trailing punctuation", () => {
  expect(withGuidance("Failed: no live session", "Retry won't deliver it twice.")).toBe(
    "Failed: no live session. Retry won't deliver it twice."
  );
  expect(withGuidance(`Failed: ${RECEIPT_TIMEOUT_CAUSE}`, "Retry won't deliver it twice.")).toBe(
    `Failed: ${RECEIPT_TIMEOUT_CAUSE} Retry won't deliver it twice.`
  );
});

// "Sending in a different mode delivers it again" is only true of a send that may already have
// reached the recipient. For a failure that never got there, nothing was delivered to deliver
// "again"; and the mention surface has no mode-change button to name at all.
test("the mode-change clause is offered only for a receipt timeout, and never on a mention", () => {
  expect(safeRetryGuidance("card", RECEIPT_TIMEOUT_CAUSE)).toBe(
    "Retry won't deliver it twice; sending in a different mode delivers it again."
  );
  expect(safeRetryGuidance("card", "no live session s1")).toBe("Retry won't deliver it twice.");
  expect(safeRetryGuidance("card", null)).toBe("Retry won't deliver it twice.");
  expect(safeRetryGuidance("mention", RECEIPT_TIMEOUT_CAUSE)).toBe("Retry won't deliver it twice.");
});

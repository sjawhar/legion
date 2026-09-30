import { expect, test } from "bun:test";
import { DELIVERY_DUPLICATE_WINDOW_MS } from "@legion/contracts";
import { createDeliveryDedupe } from "../delivery";

// The listener mints a fresh event id for every send, so a re-send of one message - a Dispatch
// Retry, an outbox re-publish, a webhook redelivery - carries the first send's dedupe key and
// nothing else of it. The host recognises the repeat by that key alone.
const attempt = { event_id: "evt-attempt", dedupe_key: "agent.ses_a.m1:aside" };
const retry = { event_id: "evt-retry", dedupe_key: "agent.ses_a.m1:aside" };
const otherMode = { event_id: "evt-other", dedupe_key: "agent.ses_a.m1:btw" };

test("a re-send under a delivered dedupe key is a repeat, whatever its event id", () => {
  const dedupe = createDeliveryDedupe();
  dedupe.remember(attempt);

  expect(dedupe.isRepeat(retry)).toBe(true);
  expect(dedupe.isRepeat(otherMode)).toBe(false);
});

test("a frame without a dedupe key is never a repeat", () => {
  const dedupe = createDeliveryDedupe();
  dedupe.remember({});

  expect(dedupe.isRepeat({})).toBe(false);
  expect(dedupe.isRepeat(undefined)).toBe(false);
});

test("a key is remembered for the duplicate window and not a moment longer", () => {
  let now = 0;
  const dedupe = createDeliveryDedupe(() => now);
  dedupe.remember(attempt);

  now = DELIVERY_DUPLICATE_WINDOW_MS - 1;
  expect(dedupe.isRepeat(retry)).toBe(true);
  now = DELIVERY_DUPLICATE_WINDOW_MS;
  expect(dedupe.isRepeat(retry)).toBe(false);
});

test("a key forgotten after a failed delivery lets its re-send through", () => {
  const dedupe = createDeliveryDedupe();
  dedupe.remember(attempt);
  dedupe.forget(attempt);

  expect(dedupe.isRepeat(retry)).toBe(false);
});

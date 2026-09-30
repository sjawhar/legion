import { expect, test } from "bun:test";
import { DELIVERY_DUPLICATE_WINDOW_MS } from "@legion/contracts";
import { createDeliveryDedupe } from "../delivery";

// The listener mints a fresh event id for every send, so a re-send of one message - a Dispatch
// Retry, an outbox re-publish, a webhook redelivery - carries the first send's dedupe key and
// nothing else of it. The host recognises the repeat by that key alone.
const attempt = { event_id: "evt-attempt", source: "dispatch", dedupe_key: "agent.ses_a.m1:aside" };
const retry = { event_id: "evt-retry", source: "dispatch", dedupe_key: "agent.ses_a.m1:aside" };
const otherMode = { event_id: "evt-other", source: "dispatch", dedupe_key: "agent.ses_a.m1:btw" };

// A claim is the test and the record at once, so a repeat that arrives while the first frame is
// still being delivered is refused before the first one's hand-off settles.
test("a re-send under a claimed dedupe key is a repeat, whatever its event id", () => {
  const dedupe = createDeliveryDedupe();

  expect(dedupe.claim(attempt)).toBe(true);
  expect(dedupe.claim(retry)).toBe(false);
  expect(dedupe.claim(otherMode)).toBe(true);
});

test("a frame without a dedupe key is never a repeat", () => {
  const dedupe = createDeliveryDedupe();

  expect(dedupe.claim({})).toBe(true);
  expect(dedupe.claim({})).toBe(true);
  expect(dedupe.claim(undefined)).toBe(true);
});

test("a key that names its event is remembered for the duplicate window and not a moment longer", () => {
  let now = 0;
  const dedupe = createDeliveryDedupe(() => now);
  dedupe.claim(attempt);

  now = DELIVERY_DUPLICATE_WINDOW_MS - 1;
  expect(dedupe.claim(retry)).toBe(false);
  now = DELIVERY_DUPLICATE_WINDOW_MS;
  expect(dedupe.claim(retry)).toBe(true);
});

// The MCP bridge keys a notification by a hash of its URI and summary, and every textless
// notification on one URI shares it; the Go daemon's outbox row ids start over in a new store.
// Distinct events share such a key, so dropping on it loses the later one: it is never a repeat.
// A key the listener minted is repeated only by a re-send of its one message, which is dropped.
test("a key that does not name its event is never a repeat, a key the listener minted is", () => {
  const dedupe = createDeliveryDedupe();
  const bridge = {
    source: "github",
    source_event_id: "mcp://acme/alerts",
    dedupe_key: "github.3f2a",
  };
  const outbox = { source: "agent", dedupe_key: "legion-outbox:1" };
  const minted = {
    source: "envoy",
    dedupe_key: "envoy.role.forward.publish.0123456789abcdef0123456789abcdef",
  };

  expect([dedupe.claim(bridge), dedupe.claim(bridge)]).toEqual([true, true]);
  expect([dedupe.claim(outbox), dedupe.claim(outbox)]).toEqual([true, true]);
  expect([dedupe.claim(minted), dedupe.claim(minted)]).toEqual([true, false]);
});

test("a claim released after a failed delivery lets its re-send through", () => {
  const dedupe = createDeliveryDedupe();
  dedupe.claim(attempt);
  dedupe.release(attempt);

  expect(dedupe.claim(retry)).toBe(true);
});

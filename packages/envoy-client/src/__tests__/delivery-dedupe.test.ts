import { expect, test } from "bun:test";
import { DELIVERY_DUPLICATE_WINDOW_MS } from "@legion/contracts";
import { createDeliveryDedupe, DELIVERY_DEDUPE_KEY_LIMIT } from "../delivery";

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

  expect(dedupe.claim(attempt)).toBeDefined();
  expect(dedupe.claim(retry)).toBeUndefined();
  expect(dedupe.claim(otherMode)).toBeDefined();
});

test("a frame without a dedupe key or an event id is never a repeat", () => {
  const dedupe = createDeliveryDedupe();

  expect(dedupe.claim({})).toBeDefined();
  expect(dedupe.claim({})).toBeDefined();
  expect(dedupe.claim(undefined)).toBeDefined();
});

// A frame that follows two overlapping subscriptions (`pr.7.>` and `pr.7.checks`) arrives once
// per subscription, every copy carrying the one event id its publish minted. A CI settlement's key
// does not name its event, so only that event id says the second copy is the first again; a later
// settlement under the same key is a new publish with its own event id, and arrives.
test("a second copy of one publish is a repeat by its event id, whatever its key", () => {
  const dedupe = createDeliveryDedupe();
  const settlement = (eventId: string) => ({
    event_id: eventId,
    source: "github",
    source_event_id: `ci-${eventId}`,
    dedupe_key: "github.checks.acme.widgets.pr.7.0a1b2c3.g1",
  });

  expect(dedupe.claim(settlement("evt-1"))).toBeDefined();
  expect(dedupe.claim(settlement("evt-1"))).toBeUndefined();
  expect(dedupe.claim(settlement("evt-2"))).toBeDefined();
});

test("a key that names its event is remembered for the duplicate window and not a moment longer", () => {
  let now = 0;
  const dedupe = createDeliveryDedupe(() => now);
  dedupe.claim(attempt);

  now = DELIVERY_DUPLICATE_WINDOW_MS - 1;
  expect(dedupe.claim(retry)).toBeUndefined();
  now = DELIVERY_DUPLICATE_WINDOW_MS;
  expect(dedupe.claim({ ...retry, event_id: "evt-retry-late" })).toBeDefined();
});

// The whole production notification stream stored 100,028 messages in one 72-hour window
// (2026-09-30), and a host is handed no more keys than the stream carries on the topics it
// follows. A host that follows all of them still recognises a Retry made at the end of the window.
test("a repeat is still recognised after as many other keys as the whole stream stored in one window", () => {
  const dedupe = createDeliveryDedupe(() => 0);
  const frame = (n: number) => ({
    event_id: `evt-${n}`,
    source: "dispatch",
    dedupe_key: `dispatch-${n}`,
  });
  dedupe.claim(frame(0));
  for (let n = 1; n <= 100_028; n++) dedupe.claim(frame(n));

  expect(dedupe.claim({ ...frame(0), event_id: "evt-0-retry" })).toBeUndefined();
});

// A flood of fresh keys inside the window cannot grow the record past its limit: the oldest key
// goes first, and every later one is still recognised. Each frame carries an event id as well, and
// those are held apart, so they do not spend the key limit.
test("past the key limit the oldest key is forgotten first, and only it", () => {
  const dedupe = createDeliveryDedupe(() => 0);
  let sends = 0;
  const send = (n: number) => ({
    event_id: `evt-${sends++}`,
    source: "dispatch",
    dedupe_key: `dispatch-${n}`,
  });
  for (let n = 0; n < DELIVERY_DEDUPE_KEY_LIMIT; n++) dedupe.claim(send(n));
  expect(dedupe.claim(send(0))).toBeUndefined();

  expect(dedupe.claim(send(DELIVERY_DEDUPE_KEY_LIMIT))).toBeDefined();
  expect(dedupe.claim(send(1))).toBeUndefined();
  expect(dedupe.claim(send(DELIVERY_DEDUPE_KEY_LIMIT))).toBeUndefined();
  expect(dedupe.claim(send(0))).toBeDefined();
});

// The MCP bridge keys a notification by a hash of its URI and summary, and every textless
// notification on one URI shares it; the Go daemon's outbox row ids start over in a new store.
// Distinct events share such a key, so dropping on it loses the later one: it is never a repeat.
// A key the listener minted is repeated only by a re-send of its one message, which is dropped.
test("a key that does not name its event is never a repeat, a key the listener minted is", () => {
  const dedupe = createDeliveryDedupe();
  const bridge = (eventId: string) => ({
    event_id: eventId,
    source: "github",
    source_event_id: "mcp://acme/alerts",
    dedupe_key: "github.3f2a",
  });
  const outbox = (eventId: string) => ({
    event_id: eventId,
    source: "agent",
    dedupe_key: "legion-outbox:1",
  });
  const minted = (eventId: string) => ({
    event_id: eventId,
    source: "envoy",
    dedupe_key: "envoy.role.forward.publish.0123456789abcdef0123456789abcdef",
  });

  expect([dedupe.claim(bridge("b1")), dedupe.claim(bridge("b2"))]).toEqual([
    expect.anything(),
    expect.anything(),
  ]);
  expect([dedupe.claim(outbox("o1")), dedupe.claim(outbox("o2"))]).toEqual([
    expect.anything(),
    expect.anything(),
  ]);
  expect(dedupe.claim(minted("m1"))).toBeDefined();
  expect(dedupe.claim(minted("m2"))).toBeUndefined();
});

test("a claim released after a failed delivery lets its re-send, and its other copy, through", () => {
  const dedupe = createDeliveryDedupe();
  dedupe.claim(attempt)?.release();

  expect(dedupe.claim(attempt)).toBeDefined();
  dedupe.claim(otherMode)?.release();
  expect(dedupe.claim({ ...otherMode, event_id: "evt-other-retry" })).toBeDefined();
});

// A frame on a followed topic can carry a recorded Dispatch key without naming it (a producer that
// is not Dispatch, or a caller who chose the key). Its claim recorded only its event id, so a host
// that skips it or answers it with an error must not unclaim the Dispatch key: the real Retry would
// then reach the agent a second time.
test("a release undoes only what its own claim recorded", () => {
  const dedupe = createDeliveryDedupe();
  dedupe.claim(attempt);

  const borrowed = dedupe.claim({
    event_id: "evt-borrowed",
    source: "agent",
    dedupe_key: attempt.dedupe_key,
  });
  expect(borrowed).toBeDefined();
  borrowed?.release();

  expect(dedupe.claim(retry)).toBeUndefined();
});

// A host can reach its release twice for one frame (an error reply that throws after releasing),
// by which time a re-send may have claimed the key again.
test("a second release of one claim leaves a later claim of the same key in place", () => {
  const dedupe = createDeliveryDedupe();
  const first = dedupe.claim(attempt);
  first?.release();
  expect(dedupe.claim(retry)).toBeDefined();

  first?.release();
  expect(dedupe.claim({ ...retry, event_id: "evt-retry-2" })).toBeUndefined();
});

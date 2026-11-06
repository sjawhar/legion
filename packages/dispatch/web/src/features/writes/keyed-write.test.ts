import { expect, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";

import { keyMinted } from "../../__tests__/kept-sends-harness";
import {
  keyExpired,
  retriesUnderItsKey,
  SendExpiredError,
  SendStoppedError,
  writeUnderKey,
} from "./keyed-write";

/** A v7 key minted at `MINTED_AT`: its first twelve hex digits are that millisecond. */
const MINTED_AT = 1_700_000_000_000;
const V7_KEY = "018bcfe5-6800-7abc-9def-0123456789ab";

test("a key is spent once Dispatch's whole key lifetime has passed since it was minted", () => {
  expect(keyExpired(V7_KEY, MINTED_AT + IDEMPOTENCY_KEY_TTL_MS)).toBe(true);
  expect(keyExpired(V7_KEY, MINTED_AT + IDEMPOTENCY_KEY_TTL_MS - 1)).toBe(false);
});

// A key minted before this release holds no mint time, and calling it spent would mint a new key
// for a write Dispatch may already have done.
test("a key with no mint time is never spent", () => {
  const key = "3f2a8b1c-5d4e-4a7b-9c2d-0e1f2a3b4c5d";
  expect(keyExpired(key, MINTED_AT + 10 * IDEMPOTENCY_KEY_TTL_MS)).toBe(false);
});

// A key Dispatch has forgotten names nothing there, so a request under it would be a second write
// of what the person pressed rather than a repeat of the first. The write makes no attempt at all
// and ends as a stopped write does, so every surface that branches on that class keeps offering
// Retry and Discard beside it.
test("a write whose key is older than the key's lifetime sends no attempt", async () => {
  let attempts = 0;
  const refusal = await writeUnderKey(
    () => {
      attempts += 1;
      return Promise.resolve({ key: "CORE-1" });
    },
    keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS)
  ).catch((error: unknown) => error);
  expect(attempts).toBe(0);
  expect(refusal).toBeInstanceOf(SendExpiredError);
  expect(refusal).toBeInstanceOf(SendStoppedError);
});

// Retry of a write that ended without Dispatch's verdict asks again under its own key, which
// Dispatch answers with what the first attempt did. Once the key is spent there is nothing to ask
// again under, so the Retry is a new write under a new key, which the store and the three
// surfaces that re-press a write (`send-request.ts`, `broadcast-composition.ts`,
// `useKeyedWrite.ts`) all read from here.
test("a Retry keeps the write's key after a stop and takes a new one once the key is spent", () => {
  expect(retriesUnderItsKey(new SendStoppedError())).toBe(true);
  expect(retriesUnderItsKey(new SendExpiredError())).toBe(false);
});

import { expect, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";

import { keyExpired } from "./keyed-write";

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

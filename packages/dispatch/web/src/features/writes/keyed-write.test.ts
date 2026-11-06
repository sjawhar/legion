import { expect, test } from "bun:test";
import { IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import { keyMinted } from "../../__tests__/kept-sends-harness";
import { ApiError } from "../../api/client";
import {
  keyExpired,
  keySpent,
  retriesUnderItsKey,
  SendExpiredError,
  SendStoppedError,
  writeUnderKey,
} from "./keyed-write";

/** A v7 key minted at `MINTED_AT`, as `newSendKey` mints one (`keyMinted`). */
const MINTED_AT = 1_700_000_000_000;
const V7_KEY = keyMinted(MINTED_AT);

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

// One question, asked in one place, of what the key has already carried: the method that went
// out under it, or `null` where nothing did. `refusalFrom` asks it of what the write's record
// says went out (`KeptWrite.sent`), and the retry loop asks it of what the
// store says the key carried and of its own attempts, so the two cannot answer the same write
// differently.
test("a key is spent for what went out under it, never for a key that carried nothing", () => {
  const spent = keyMinted(MINTED_AT);
  const now = MINTED_AT + IDEMPOTENCY_KEY_TTL_MS;
  expect(keySpent("POST", spent, now)).toBe(true);
  expect(keySpent("PATCH", spent, now)).toBe(true);
  expect(keySpent("PUT", spent, now)).toBe(false);
  expect(keySpent("DELETE", spent, now)).toBe(false);
  expect(keySpent("POST", spent, now - 1)).toBe(false);
  // Nothing went out under it: Dispatch has never seen the key, so there is nothing under it
  // that a request now would write a second time.
  expect(keySpent(null, spent, now)).toBe(false);
});

// A key Dispatch has forgotten, which a `POST` already went out under, names nothing there any
// more: a request under it would be a second write of what the person pressed rather than a
// repeat of the first. The write makes no attempt at all and ends as a stopped write does, so
// every surface that branches on that class keeps offering Retry and Discard beside it.
test("a write whose key is older than the key's lifetime sends no attempt", async () => {
  let attempts = 0;
  const refusal = await writeUnderKey(
    () => {
      attempts += 1;
      return Promise.resolve({ key: "CORE-1" });
    },
    keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS),
    { carried: "POST" }
  ).catch((error: unknown) => error);
  expect(attempts).toBe(0);
  expect(refusal).toBeInstanceOf(SendExpiredError);
  expect(refusal).toBeInstanceOf(SendStoppedError);
});

// A write whose key has carried nothing sent nothing, whatever its age: a page that kept it
// across a reload without ever sending it, whose Retry comes a day after the press. Dispatch
// has never seen the key, so the attempt still to come is the first one and it goes out - it is
// not held back to be refused, and then sent only on a second Retry under a new key.
test("a write under a key that has carried nothing makes its attempt a day later", async () => {
  let attempts = 0;
  const answer = await writeUnderKey(
    () => {
      attempts += 1;
      return Promise.resolve({ key: "CORE-1" });
    },
    keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS),
    { carried: null }
  );
  expect(attempts).toBe(1);
  expect(answer).toEqual({ key: "CORE-1" });
});

// The key carries from the moment its first request goes out, which the loop is told as it goes:
// that request may have reached Dispatch even where the attempt ended without an answer, so the
// attempt after it is the one the spent key stops.
test("a key spent before the write began carries its first attempt and stops the second", async () => {
  let attempts = 0;
  const refusal = await writeUnderKey(
    (bounds) => {
      attempts += 1;
      bounds.sent?.({ body: { title: "A thing" }, method: "POST", path: "/api/v1/issues" });
      return Promise.reject(new ApiError(503, { code: "UNAVAILABLE", error: "down" }));
    },
    keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS),
    { carried: null }
  ).catch((error: unknown) => error);
  expect(attempts).toBe(1);
  expect(refusal).toBeInstanceOf(SendExpiredError);
});

// The key stops only the write that sends it. A `PUT` or a `DELETE` sends none, so Dispatch
// keeps nothing under one for it and making it again is the same change: a tab that slept past
// the key's lifetime still makes it. A key a `POST` went out under is the write the guard is
// for, and the write is refused before any attempt.
test("a spent key stops a write that sends it and lets one that does not through", async () => {
  const spent = keyMinted(Date.now() - IDEMPOTENCY_KEY_TTL_MS);
  let attempts = 0;
  const answer = await writeUnderKey(
    () => {
      attempts += 1;
      return Promise.resolve({ seq: 2 });
    },
    spent,
    {
      carried: "PUT",
      planned: { body: { seq: 2 }, method: "PUT", path: "/api/v1/me/state/CORE-1" },
    }
  );
  expect(attempts).toBe(1);
  expect(answer).toEqual({ seq: 2 });

  const refusal = await writeUnderKey(() => Promise.resolve({}), spent, {
    carried: "POST",
    planned: { body: { body: "hello" }, method: "POST", path: "/api/v1/issues/CORE-1/comments" },
  }).catch((error: unknown) => error);
  expect(refusal).toBeInstanceOf(SendExpiredError);
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

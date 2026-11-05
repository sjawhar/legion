import { expect, test } from "bun:test";

import { keyMintedAt, newSendKey } from "./send-key";

// A browser exposes `crypto.randomUUID` only in a secure context, so on a plain-HTTP origin reached
// by a non-loopback name it is undefined, and a key built from it would throw on the first send
// (the Agents page builds its broadcast's key on its first render).
test("a send key is a fresh v7 UUID even where crypto.randomUUID is missing", () => {
  const own = Object.getOwnPropertyDescriptor(crypto, "randomUUID");
  Object.defineProperty(crypto, "randomUUID", {
    configurable: true,
    value: undefined,
    writable: true,
  });
  try {
    const keys = Array.from({ length: 64 }, () => newSendKey());
    for (const key of keys) {
      expect(key).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    }
    expect(new Set(keys).size).toBe(keys.length);
  } finally {
    if (own === undefined) Reflect.deleteProperty(crypto, "randomUUID");
    else Object.defineProperty(crypto, "randomUUID", own);
  }
});

test("a send key carries the millisecond it was minted", () => {
  const before = Date.now();
  const minted = keyMintedAt(newSendKey());
  expect(minted).toBeGreaterThanOrEqual(before);
  expect(minted).toBeLessThanOrEqual(Date.now());
});

// Every key the shipping release minted is a v4, and a page reloaded after this release still
// carries those: they hold no mint time to read.
test("a v4 key carries no mint time", () => {
  expect(keyMintedAt("3f2a8b1c-5d4e-4a7b-9c2d-0e1f2a3b4c5d")).toBeUndefined();
});

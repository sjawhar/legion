import { expect, test } from "bun:test";

import { newSendKey } from "./broadcast-composition";

// A browser exposes `crypto.randomUUID` only in a secure context, so on a plain-HTTP origin reached
// by a non-loopback name it is undefined, and a key built from it would throw on the Agents page's
// first render.
test("a send key is a fresh v4 UUID even where crypto.randomUUID is missing", () => {
  const own = Object.getOwnPropertyDescriptor(crypto, "randomUUID");
  Object.defineProperty(crypto, "randomUUID", {
    configurable: true,
    value: undefined,
    writable: true,
  });
  try {
    const keys = Array.from({ length: 64 }, () => newSendKey());
    for (const key of keys) {
      expect(key).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    }
    expect(new Set(keys).size).toBe(keys.length);
  } finally {
    if (own === undefined) Reflect.deleteProperty(crypto, "randomUUID");
    else Object.defineProperty(crypto, "randomUUID", own);
  }
});

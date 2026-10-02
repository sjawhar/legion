import { expect, test } from "bun:test";
import { mintBlockId, setBlockIdGenerator } from "../src/editor/schema/block-ids";
import { uuidV4 } from "../src/editor/schema/uuid";

const v4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** Runs `body` with `crypto[name]` replaced, then restores the own property or its absence. */
function withCryptoProperty<T>(
  name: "getRandomValues" | "randomUUID",
  value: unknown,
  body: () => T
): T {
  const own = Object.getOwnPropertyDescriptor(crypto, name);
  Object.defineProperty(crypto, name, { configurable: true, value, writable: true });
  try {
    return body();
  } finally {
    if (own === undefined) Reflect.deleteProperty(crypto, name);
    else Object.defineProperty(crypto, name, own);
  }
}

// A browser defines `crypto.randomUUID` only in a secure context; a plain-HTTP origin reached by a
// non-loopback name has none, and the default generator must mint there.
test("the default block id generator mints fresh v4 UUIDs where crypto.randomUUID is missing", () => {
  withCryptoProperty("randomUUID", undefined, () => {
    setBlockIdGenerator(null);
    const ids = Array.from({ length: 64 }, () => mintBlockId());
    for (const id of ids) expect(id).toMatch(v4);
    expect(new Set(ids).size).toBe(ids.length);
  });
});

test("uuidV4 sets the version and variant bits and nothing else", () => {
  const filled = (byte: number) =>
    withCryptoProperty("getRandomValues", (array: Uint8Array) => array.fill(byte), uuidV4);
  expect(filled(0x00)).toBe("00000000-0000-4000-8000-000000000000");
  expect(filled(0xff)).toBe("ffffffff-ffff-4fff-bfff-ffffffffffff");
});

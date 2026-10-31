import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { sha256Hex } from "./sha256";

// Lengths around each padding boundary: a message whose length and its 9 bytes of padding fill a
// block exactly (55), spill into a second (56), and fill one or more whole blocks (64, 128).
test("digests every length as node:crypto's SHA-256 does", () => {
  for (const length of [0, 1, 55, 56, 63, 64, 65, 119, 120, 128, 1000, 5000]) {
    const bytes = Uint8Array.from({ length }, (_, i) => (i * 131) % 256);
    expect(sha256Hex(bytes)).toBe(createHash("sha256").update(bytes).digest("hex"));
  }
});

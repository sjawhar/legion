import { describe, expect, it } from "bun:test";
import { decodeBase62, encodeBase62, generateShortId, uuidToShort } from "../short-id";

describe("Base62", () => {
  describe("encodeBase62", () => {
    it("encodes zero", () => {
      expect(encodeBase62(0n)).toBe("0");
    });

    it("encodes single digits", () => {
      expect(encodeBase62(9n)).toBe("9");
      expect(encodeBase62(10n)).toBe("A");
      expect(encodeBase62(35n)).toBe("Z");
      expect(encodeBase62(36n)).toBe("a");
      expect(encodeBase62(61n)).toBe("z");
    });

    it("encodes multi-digit numbers", () => {
      expect(encodeBase62(62n)).toBe("10");
      expect(encodeBase62(124n)).toBe("20");
    });
  });

  describe("decodeBase62", () => {
    it("decodes single digits", () => {
      expect(decodeBase62("0")).toBe(0n);
      expect(decodeBase62("9")).toBe(9n);
      expect(decodeBase62("A")).toBe(10n);
      expect(decodeBase62("Z")).toBe(35n);
      expect(decodeBase62("a")).toBe(36n);
      expect(decodeBase62("z")).toBe(61n);
    });

    it("decodes multi-digit numbers", () => {
      expect(decodeBase62("10")).toBe(62n);
      expect(decodeBase62("20")).toBe(124n);
    });

    it("roundtrips encode/decode", () => {
      const testValues = [0n, 1n, 61n, 62n, 1000n, 1000000n, 2n ** 64n];
      for (const n of testValues) {
        expect(decodeBase62(encodeBase62(n))).toBe(n);
      }
    });
  });
});

describe("uuidToShort", () => {
  it("converts UUID with hyphens", () => {
    const uuid = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const short = uuidToShort(uuid);
    expect(short.length).toBe(8);
    expect(/^[0-9A-Za-z]+$/.test(short)).toBe(true);
  });

  it("converts UUID without hyphens", () => {
    const uuid = "7b4f0862b7754cb09a6785400c6f44a8";
    const short = uuidToShort(uuid);
    expect(short.length).toBe(8);
    expect(/^[0-9A-Za-z]+$/.test(short)).toBe(true);
  });

  it("produces same short ID for UUID with and without hyphens", () => {
    const uuid1 = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const uuid2 = "7b4f0862b7754cb09a6785400c6f44a8";
    expect(uuidToShort(uuid1)).toBe(uuidToShort(uuid2));
  });

  it("produces different short IDs for different UUIDs", () => {
    const uuid1 = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    const uuid2 = "00000000-0000-0000-0000-000000000000";
    expect(uuidToShort(uuid1)).not.toBe(uuidToShort(uuid2));
  });

  it("respects custom length parameter", () => {
    const uuid = "7b4f0862-b775-4cb0-9a67-85400c6f44a8";
    expect(uuidToShort(uuid, 4).length).toBe(4);
    expect(uuidToShort(uuid, 12).length).toBe(12);
  });
});

describe("generateShortId", () => {
  it("generates a short ID with default length", () => {
    const id = generateShortId();
    expect(id.length).toBe(8);
    expect(/^[0-9A-Za-z]+$/.test(id)).toBe(true);
  });

  it("generates a short ID with custom length", () => {
    const id = generateShortId(12);
    expect(id.length).toBe(12);
    expect(/^[0-9A-Za-z]+$/.test(id)).toBe(true);
  });

  it("generates different IDs on successive calls", () => {
    const id1 = generateShortId();
    const id2 = generateShortId();
    expect(id1).not.toBe(id2);
  });
});

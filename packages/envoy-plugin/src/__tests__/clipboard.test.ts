import { afterEach, beforeEach, describe, expect, it, mock } from "bun:test";
import { copyOsc52 } from "../clipboard";

describe("copyOsc52", () => {
  let writes: string[];
  let originalWrite: typeof process.stdout.write;

  beforeEach(() => {
    writes = [];
    originalWrite = process.stdout.write;
    process.stdout.write = mock((chunk: string | Uint8Array) => {
      writes.push(typeof chunk === "string" ? chunk : Buffer.from(chunk).toString("utf-8"));
      return true;
    }) as typeof process.stdout.write;
  });

  afterEach(() => {
    process.stdout.write = originalWrite;
  });

  it("writes the expected OSC 52 escape sequence for ASCII", () => {
    const ok = copyOsc52("ses_abc");
    expect(ok).toBe(true);
    expect(writes).toEqual(["\x1b]52;c;c2VzX2FiYw==\x07"]);
  });

  it("writes the empty payload escape for an empty string", () => {
    const ok = copyOsc52("");
    expect(ok).toBe(true);
    expect(writes).toEqual(["\x1b]52;c;\x07"]);
  });

  it("base64-encodes UTF-8 bytes, not Latin-1 / not JS code units", () => {
    const ok = copyOsc52("café");
    expect(ok).toBe(true);
    // UTF-8 bytes for "café" = 63 61 66 c3 a9 → base64 "Y2Fmw6k="
    expect(writes).toEqual(["\x1b]52;c;Y2Fmw6k=\x07"]);
  });

  it("returns false when process.stdout.write throws", () => {
    process.stdout.write = mock(() => {
      throw new Error("stdout closed");
    }) as typeof process.stdout.write;
    const ok = copyOsc52("anything");
    expect(ok).toBe(false);
  });
});

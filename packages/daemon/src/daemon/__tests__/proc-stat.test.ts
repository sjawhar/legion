import { describe, expect, it } from "bun:test";
import { parseProcStatStartTicks } from "../proc-stat";

// Fields 3..52 of a real `/proc/<pid>/stat` line for a shell; field 22 (starttime) is 1234567.
const TAIL =
  "S 1 4242 4242 0 -1 4194560 812 0 0 0 3 1 0 0 20 0 1 0 1234567 8912896 486 18446744073709551615 1 1 0 0 0 0 0 0 65536 1 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0 0";

describe("parseProcStatStartTicks", () => {
  it("reads field 22 (starttime) from an ordinary stat line", () => {
    expect(parseProcStatStartTicks(`4242 (sh) ${TAIL}\n`)).toBe(1234567);
  });

  it("counts fields only after the last `)`, so a comm containing spaces or parentheses cannot shift them", () => {
    expect(parseProcStatStartTicks(`4242 (tmux: server) ${TAIL}`)).toBe(1234567);
    expect(parseProcStatStartTicks(`4242 (a) b (c)) ${TAIL}`)).toBe(1234567);
  });

  it("throws on a line with no comm terminator or too few fields, never returning a guess", () => {
    expect(() => parseProcStatStartTicks("4242 sh S 1 2")).toThrow(/comm terminator/);
    expect(() => parseProcStatStartTicks("4242 (sh) S 1 2 3")).toThrow(/starttime/);
  });

  // Proves the last-`)` split and the field index hold against a real kernel line, not only
  // the hand-written one above.
  it("parses this process's own /proc/self/stat line", async () => {
    const stat = await Bun.file("/proc/self/stat").text();
    expect(parseProcStatStartTicks(stat)).toBeGreaterThan(0);
  });
});

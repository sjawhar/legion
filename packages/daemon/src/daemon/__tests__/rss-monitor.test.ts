import { describe, expect, it } from "bun:test";
import { readProcessRssBytes } from "../rss-monitor";

describe("readProcessRssBytes", () => {
  it("parses /proc/<pid>/statm and returns RSS in bytes", () => {
    // statm format: size resident shared text lib data dt (all in pages)
    // 100000 pages * 4096 bytes/page = 409600000 bytes
    const fakeStatm = "200000 100000 5000 1000 0 50000 0";
    const result = readProcessRssBytes(1, () => fakeStatm);
    expect(result).toBe(100000 * 4096);
  });

  it("returns null when file read fails", () => {
    const result = readProcessRssBytes(99999, () => {
      throw new Error("No such file or directory");
    });
    expect(result).toBeNull();
  });

  it("returns null when resident field is not a number", () => {
    const result = readProcessRssBytes(1, () => "200000 NaN 5000");
    expect(result).toBeNull();
  });

  it("returns null when statm has insufficient fields", () => {
    const result = readProcessRssBytes(1, () => "200000");
    expect(result).toBeNull();
  });

  it("handles large RSS values (20GB+)", () => {
    // 20GB = 20 * 1024 * 1024 * 1024 bytes = 5242880 pages (at 4096 bytes/page)
    const pages = Math.ceil((20 * 1024 * 1024 * 1024) / 4096);
    const fakeStatm = `500000 ${pages} 5000 1000 0 50000 0`;
    const result = readProcessRssBytes(1, () => fakeStatm);
    expect(result).toBe(pages * 4096);
    // Verify it's approximately 20GB
    if (result !== null) {
      expect(result / 1024 / 1024 / 1024).toBeCloseTo(20, 0);
    }
  });

  it("returns null for negative resident pages", () => {
    const result = readProcessRssBytes(1, () => "200000 -100 5000");
    expect(result).toBeNull();
  });

  it("handles extra whitespace in statm", () => {
    const fakeStatm = "  200000  100000  5000  1000  0  50000  0  ";
    const result = readProcessRssBytes(1, () => fakeStatm);
    expect(result).toBe(100000 * 4096);
  });

  it("reads actual /proc/self/statm when no readFile override", () => {
    // This tests the real path on Linux
    const result = readProcessRssBytes(process.pid);
    if (process.platform === "linux") {
      expect(result).not.toBeNull();
      if (result !== null) {
        expect(result).toBeGreaterThan(0);
      }
    } else {
      // On non-Linux, /proc doesn't exist
      expect(result).toBeNull();
    }
  });
});

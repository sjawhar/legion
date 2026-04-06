import { describe, expect, it, mock } from "bun:test";
import { resolvePort } from "../port";

describe("resolvePort", () => {
  const noopExec = (() => {
    throw new Error("ss not available");
  }) as typeof import("node:child_process").execFileSync;

  const ssOutput = (pid: number, port: number) =>
    [
      "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process",
      `LISTEN 0      511    127.0.0.1:${port}  0.0.0.0:*  users:(("bun",pid=${pid},fd=6))`,
      "",
    ].join("\n");

  describe("URL port extraction", () => {
    it("returns port from standard non-default URL", () => {
      expect(resolvePort(new URL("http://127.0.0.1:4096"), noopExec)).toBe(4096);
    });

    it("returns port for high serve ports", () => {
      expect(resolvePort(new URL("http://127.0.0.1:13381"), noopExec)).toBe(13381);
    });

    it("returns port for localhost URLs", () => {
      expect(resolvePort(new URL("http://localhost:4096"), noopExec)).toBe(4096);
    });

    it("returns port for IPv6 URLs", () => {
      expect(resolvePort(new URL("http://[::1]:4096"), noopExec)).toBe(4096);
    });
  });

  describe("ss fallback", () => {
    it("uses ss when URL has no port (default HTTP)", () => {
      const exec = mock(() => ssOutput(process.pid, 4096));
      expect(resolvePort(new URL("http://127.0.0.1"), exec as never)).toBe(4096);
      expect(exec).toHaveBeenCalledWith("ss", ["-tlnp"], { encoding: "utf-8" });
    });

    it("uses ss when URL port is 0", () => {
      // URL("http://127.0.0.1:0").port is "0", which is not > 0
      const exec = mock(() => ssOutput(process.pid, 13381));
      expect(resolvePort(new URL("http://127.0.0.1:0"), exec as never)).toBe(13381);
    });

    it("ignores ss lines with different PIDs", () => {
      const exec = mock(() => ssOutput(99999, 4096));
      expect(resolvePort(new URL("http://127.0.0.1"), exec as never)).toBeNull();
    });

    it("handles multiple ss entries and picks the matching PID", () => {
      const output = [
        "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process",
        `LISTEN 0      511    127.0.0.1:8080  0.0.0.0:*  users:(("node",pid=99999,fd=6))`,
        `LISTEN 0      511    127.0.0.1:4096  0.0.0.0:*  users:(("bun",pid=${process.pid},fd=7))`,
        "",
      ].join("\n");
      const exec = mock(() => output);
      expect(resolvePort(new URL("http://127.0.0.1"), exec as never)).toBe(4096);
    });

    it("handles IPv6 listening addresses in ss output", () => {
      const output = [
        "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process",
        `LISTEN 0      511    [::]:4096  [::]:*  users:(("bun",pid=${process.pid},fd=6))`,
        "",
      ].join("\n");
      const exec = mock(() => output);
      expect(resolvePort(new URL("http://127.0.0.1"), exec as never)).toBe(4096);
    });
  });
  describe("failure cases", () => {
    it("returns null when ss is not available", () => {
      expect(resolvePort(new URL("http://127.0.0.1"), noopExec)).toBeNull();
    });

    it("returns null when ss returns empty output", () => {
      const exec = mock(() => "");
      expect(resolvePort(new URL("http://127.0.0.1"), exec as never)).toBeNull();
    });
  });

  describe("edge cases", () => {
    it("URL.port is empty string for default HTTP port 80", () => {
      // http://127.0.0.1:80 → URL.port is "" (80 is default for http)
      const url = new URL("http://127.0.0.1:80");
      expect(url.port).toBe("");
      const exec = mock(() => ssOutput(process.pid, 4096));
      expect(resolvePort(url, exec as never)).toBe(4096);
    });

    it("URL.port is empty string for default HTTPS port 443", () => {
      const url = new URL("https://127.0.0.1:443");
      expect(url.port).toBe("");
      const exec = mock(() => ssOutput(process.pid, 4096));
      expect(resolvePort(url, exec as never)).toBe(4096);
    });

    it("does not use ss fallback when URL port is valid", () => {
      const exec = mock(() => ssOutput(process.pid, 9999));
      expect(resolvePort(new URL("http://127.0.0.1:4096"), exec as never)).toBe(4096);
      expect(exec).not.toHaveBeenCalled();
    });
  });
});

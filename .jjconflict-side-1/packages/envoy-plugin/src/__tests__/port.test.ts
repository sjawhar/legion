import { describe, expect, it, mock } from "bun:test";
import { resolvePort } from "../port";

describe("resolvePort", () => {
  const noopExec = (async () => {
    throw new Error("ss not available");
  }) as Parameters<typeof resolvePort>[1];

  const ssOutput = (pid: number, port: number) =>
    [
      "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process",
      `LISTEN 0      511    127.0.0.1:${port}  0.0.0.0:*  users:(("bun",pid=${pid},fd=6))`,
      "",
    ].join("\n");

  describe("URL port extraction", () => {
    it("returns port from standard non-default URL", async () => {
      expect(await resolvePort(new URL("http://127.0.0.1:4096"), noopExec)).toBe(4096);
    });

    it("returns port for high serve ports", async () => {
      expect(await resolvePort(new URL("http://127.0.0.1:13381"), noopExec)).toBe(13381);
    });

    it("returns port for localhost URLs", async () => {
      expect(await resolvePort(new URL("http://localhost:4096"), noopExec)).toBe(4096);
    });

    it("returns port for IPv6 URLs", async () => {
      expect(await resolvePort(new URL("http://[::1]:4096"), noopExec)).toBe(4096);
    });

    it("skips exec entirely when URL port is valid", async () => {
      const exec = mock(async () => ssOutput(process.pid, 9999));
      expect(await resolvePort(new URL("http://127.0.0.1:4096"), exec as never)).toBe(4096);
      expect(exec).not.toHaveBeenCalled();
    });
  });

  describe("ss fallback", () => {
    it("uses ss when URL has no port (default HTTP)", async () => {
      const exec = mock(async () => ssOutput(process.pid, 4096));
      expect(await resolvePort(new URL("http://127.0.0.1"), exec as never)).toBe(4096);
      expect(exec).toHaveBeenCalledWith("ss", ["-tlnp"], { encoding: "utf-8" });
    });

    it("uses ss when URL port is 0", async () => {
      // URL("http://127.0.0.1:0").port is "0", which is not > 0
      const exec = mock(async () => ssOutput(process.pid, 13381));
      expect(await resolvePort(new URL("http://127.0.0.1:0"), exec as never)).toBe(13381);
    });

    it("ignores ss lines with different PIDs", async () => {
      const exec = mock(async () => ssOutput(99999, 4096));
      expect(await resolvePort(new URL("http://127.0.0.1"), exec as never)).toBeNull();
    });

    it("handles multiple ss entries and picks the matching PID", async () => {
      const output = [
        "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process",
        `LISTEN 0      511    127.0.0.1:8080  0.0.0.0:*  users:(("node",pid=99999,fd=6))`,
        `LISTEN 0      511    127.0.0.1:4096  0.0.0.0:*  users:(("bun",pid=${process.pid},fd=7))`,
        "",
      ].join("\n");
      const exec = mock(async () => output);
      expect(await resolvePort(new URL("http://127.0.0.1"), exec as never)).toBe(4096);
    });

    it("handles IPv6 listening addresses in ss output", async () => {
      const output = [
        "State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process",
        `LISTEN 0      511    [::]:4096  [::]:*  users:(("bun",pid=${process.pid},fd=6))`,
        "",
      ].join("\n");
      const exec = mock(async () => output);
      expect(await resolvePort(new URL("http://127.0.0.1"), exec as never)).toBe(4096);
    });

    it("works with async exec that resolves after a delay", async () => {
      const exec = mock(
        () =>
          new Promise<string>((resolve) =>
            setTimeout(() => resolve(ssOutput(process.pid, 5555)), 10)
          )
      );
      expect(await resolvePort(new URL("http://127.0.0.1"), exec as never)).toBe(5555);
    });
  });

  describe("failure cases", () => {
    it("returns null when ss is not available", async () => {
      expect(await resolvePort(new URL("http://127.0.0.1"), noopExec)).toBeNull();
    });

    it("returns null when ss returns empty output", async () => {
      const exec = mock(async () => "");
      expect(await resolvePort(new URL("http://127.0.0.1"), exec as never)).toBeNull();
    });

    it("returns null when async exec rejects", async () => {
      const exec = mock(async () => {
        throw new Error("command failed");
      });
      expect(await resolvePort(new URL("http://127.0.0.1"), exec as never)).toBeNull();
    });
  });

  describe("edge cases", () => {
    it("URL.port is empty string for default HTTP port 80", async () => {
      // http://127.0.0.1:80 → URL.port is "" (80 is default for http)
      const url = new URL("http://127.0.0.1:80");
      expect(url.port).toBe("");
      const exec = mock(async () => ssOutput(process.pid, 4096));
      expect(await resolvePort(url, exec as never)).toBe(4096);
    });

    it("URL.port is empty string for default HTTPS port 443", async () => {
      const url = new URL("https://127.0.0.1:443");
      expect(url.port).toBe("");
      const exec = mock(async () => ssOutput(process.pid, 4096));
      expect(await resolvePort(url, exec as never)).toBe(4096);
    });
  });
});

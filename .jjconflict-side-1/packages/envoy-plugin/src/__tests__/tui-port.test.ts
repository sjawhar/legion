import { describe, expect, it } from "bun:test";
import {
  parsePort,
  resolveCurrentProcessPort,
  resolveSessionProcessPort,
  resolveTuiPort,
} from "../tui-port";

describe("parsePort", () => {
  it("returns the port from a standard http URL", () => {
    expect(parsePort("http://localhost:4096")).toBe(4096);
  });

  it("returns the port from a 127.0.0.1 URL", () => {
    expect(parsePort("http://127.0.0.1:13381")).toBe(13381);
  });

  it("returns the port from an https URL with explicit port", () => {
    expect(parsePort("https://example.com:8443")).toBe(8443);
  });

  it("returns null when the URL has no explicit port", () => {
    expect(parsePort("http://localhost")).toBe(null);
  });

  it("returns null for the empty string", () => {
    expect(parsePort("")).toBe(null);
  });

  it("returns null for a non-URL string", () => {
    expect(parsePort("not a url")).toBe(null);
  });

  it("returns null for a malformed port", () => {
    expect(parsePort("http://localhost:abc")).toBe(null);
  });

  it("returns null for undefined input", () => {
    expect(parsePort(undefined)).toBe(null);
  });
});

describe("resolveCurrentProcessPort", () => {
  it("returns a listening port for the current process from ss output", () => {
    const exec = () =>
      `LISTEN 0 512 127.0.0.1:41895 0.0.0.0:* users:(("opencode",pid=${process.pid},fd=23))`;

    expect(resolveCurrentProcessPort(exec)).toBe(41895);
  });

  it("returns null when ss has no current process match", () => {
    const exec = () => 'LISTEN 0 512 127.0.0.1:41895 0.0.0.0:* users:(("opencode",pid=123,fd=23))';

    expect(resolveCurrentProcessPort(exec)).toBe(null);
  });
});

describe("resolveTuiPort", () => {
  it("prefers the base URL port", () => {
    const exec = () => {
      throw new Error("should not call ss");
    };

    expect(resolveTuiPort("http://127.0.0.1:4096", undefined, exec)).toBe(4096);
  });

  it("falls back to the current process listening port", () => {
    const exec = () =>
      `LISTEN 0 512 127.0.0.1:35291 0.0.0.0:* users:(("opencode",pid=${process.pid},fd=23))`;

    expect(resolveTuiPort(undefined, undefined, exec)).toBe(35291);
  });
});

describe("resolveSessionProcessPort", () => {
  it("resolves the listening port for an opencode session process", () => {
    const sessionID = "ses_test";
    const exec = (command: string) => {
      if (command === "ps") return `123 opencode --port 0 -s ${sessionID}`;
      return 'LISTEN 0 512 127.0.0.1:42823 0.0.0.0:* users:(("opencode",pid=123,fd=23))';
    };

    expect(resolveSessionProcessPort(sessionID, exec)).toBe(42823);
  });
});

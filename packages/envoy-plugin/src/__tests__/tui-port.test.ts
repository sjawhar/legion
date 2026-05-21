import { describe, expect, it } from "bun:test";
import { parsePort } from "../tui-port";

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

import { describe, expect, it } from "bun:test";
import { createServer } from "node:net";
import { isPortFree, PortAllocator } from "../ports";

describe("PortAllocator", () => {
  it("allocates sequential ports from base", () => {
    const allocator = new PortAllocator(2000, 3);
    expect(allocator.allocate()).toBe(2000);
    expect(allocator.allocate()).toBe(2001);
    expect(allocator.allocate()).toBe(2002);
  });

  it("reuses released ports", () => {
    const allocator = new PortAllocator(3000, 3);
    const first = allocator.allocate();
    const second = allocator.allocate();
    allocator.release(first);
    const reused = allocator.allocate();
    expect(reused).toBe(first);
    expect(reused).not.toBe(second);
  });

  it("tracks allocation state", () => {
    const allocator = new PortAllocator(4000, 2);
    const port = allocator.allocate();
    expect(allocator.isAllocated(port)).toBe(true);
    allocator.release(port);
    expect(allocator.isAllocated(port)).toBe(false);
  });

  it("uses default base port when omitted", () => {
    const allocator = new PortAllocator();
    expect(allocator.allocate()).toBe(13381);
  });

  it("throws when allocation exceeds max ports", () => {
    const allocator = new PortAllocator(5000, 2);
    allocator.allocate();
    allocator.allocate();
    expect(() => allocator.allocate()).toThrow();
  });
});

describe("isPortFree", () => {
  it("returns true when port is not in use", async () => {
    const server = createServer();
    await new Promise<void>((resolve) => {
      server.listen(0, "127.0.0.1", () => resolve());
    });
    const address = server.address();
    if (!address || typeof address === "string") {
      throw new Error("Failed to obtain test port");
    }
    const port = address.port;
    await new Promise<void>((resolve) => server.close(() => resolve()));

    const isFree = await isPortFree(port);
    expect(isFree).toBe(true);
  });

  it("returns false when port is in use", async () => {
    const server = createServer();
    await new Promise<void>((resolve) => {
      server.listen(0, "127.0.0.1", () => resolve());
    });
    const address = server.address();
    if (!address || typeof address === "string") {
      throw new Error("Failed to obtain test port");
    }
    const port = address.port;

    const isFree = await isPortFree(port);
    expect(isFree).toBe(false);
    await new Promise<void>((resolve) => server.close(() => resolve()));
  });
});

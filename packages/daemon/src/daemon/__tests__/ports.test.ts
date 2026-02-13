import { describe, expect, it } from "bun:test";
import { PortAllocator } from "../ports";

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

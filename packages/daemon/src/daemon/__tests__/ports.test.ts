import { describe, expect, it } from "bun:test";
import { createServer } from "node:net";
import { isPortFree } from "../ports";

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

import { describe, expect, test } from "bun:test";
import { createServer } from "node:net";
import { freePorts } from "../../test/envoy-stack";

// The ports the real-stack harness gives one stack (natsd's client port, natsd's monitoring port
// and the listener's) must differ from each other, and each must be free for its process to bind.
//
// Choosing them one at a time, each probe socket closed before the next is opened, lets the kernel
// hand the same ephemeral port out twice: natsd then binds it and the listener's own bind fails
// with `listen tcp 127.0.0.1:<port>: bind: address already in use`, which is how CI's
// envoy-session-stall lane failed at f41a4d8c (run 41167627270, job 124443217959). Measured on
// this checkout's own probe, 1 triple of 2,000 repeated a port.
describe("the ports a stack is given", () => {
  // Measured in a 2-CPU pod: 0.68 s idle, 5.93 s at most under 16 busy loops (8 x nproc), so bun's
  // unwritten 5,000 ms per-test default timed this row out on a busy machine, 4 runs of 20. The
  // 2,000 rounds are what the probe above measured a reused port at, so they stay and the limit
  // becomes an explicit 60 s hang guard: it fires only if a bind or a close stops answering.
  test("differ from each other, over enough rounds to catch the kernel reusing one", async () => {
    for (let round = 0; round < 2000; round++) {
      const ports = await freePorts(3);
      expect(ports).toHaveLength(3);
      expect(new Set(ports).size).toBe(3);
    }
  }, 60_000);

  test("are free when they are handed back, so each process can bind its own", async () => {
    const ports = await freePorts(3);
    for (const port of ports) {
      const server = createServer();
      const bound = Promise.withResolvers<void>();
      server.once("error", bound.reject);
      server.listen(port, "127.0.0.1", () => bound.resolve());
      await bound.promise;
      const closed = Promise.withResolvers<void>();
      server.close(() => closed.resolve());
      await closed.promise;
    }
  });
});

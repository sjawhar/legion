import { describe, expect, it, spyOn } from "bun:test";
import { createCircuitBreakerHook } from "../circuit-breaker";

function makeInput(tool: string, sessionID: string, args: unknown = {}) {
  return { tool, sessionID, callID: "c-1", args };
}

function makeOutput(args: unknown = {}) {
  return { args };
}

describe("createCircuitBreakerHook", () => {
  describe("threshold triggering", () => {
    it("does not trigger on 4 identical calls (below default threshold of 5)", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });

    it("triggers on the 5th identical call (reaches threshold)", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow(/repetitive tool use detected/i);
    });

    it("uses default threshold of 5 when not configured", () => {
      const hook = createCircuitBreakerHook();
      const args = { filePath: "/foo" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("read", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("read", "s-1", args), makeOutput(args))
      ).toThrow();
    });

    it("respects custom threshold", () => {
      const hook = createCircuitBreakerHook({ threshold: 3 });
      const args = { command: "pwd" };

      for (let i = 0; i < 2; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();
    });

    it("continues to trigger on subsequent calls after threshold", () => {
      const hook = createCircuitBreakerHook({ threshold: 3 });
      const args = { command: "ls" };

      // Reach threshold
      for (let i = 0; i < 2; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      // 3rd, 4th, 5th calls all throw
      for (let i = 0; i < 3; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).toThrow(/repetitive tool use detected/i);
      }
    });
  });

  describe("per-session isolation", () => {
    it("tracks sessions independently — 5 calls across 2 sessions does not trigger either", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // 3 calls in s-1, 2 calls in s-2 — neither reaches threshold
      for (let i = 0; i < 3; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }
      for (let i = 0; i < 2; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args));
      }

      // 4th call in s-1 — still below threshold
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).not.toThrow();
    });

    it("triggers independently per session without affecting the other", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Fill s-1 to threshold
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();

      // s-2 is unaffected — 4 calls should not trigger
      for (let i = 0; i < 4; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args))
        ).not.toThrow();
      }
    });
  });

  describe("args normalization", () => {
    it("two calls with same args in different key order count as the same call", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });

      // 4 calls with one key order
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](
          makeInput("read", "s-1", { path: "/foo", limit: 100 }),
          makeOutput({ path: "/foo", limit: 100 })
        );
      }

      // 5th call with different key order — should still trigger (same logical args)
      expect(() =>
        hook["tool.execute.before"](
          makeInput("read", "s-1", { limit: 100, path: "/foo" }),
          makeOutput({ limit: 100, path: "/foo" })
        )
      ).toThrow();
    });

    it("calls with different arg values are distinct and do not trigger", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });

      for (let i = 0; i < 10; i++) {
        const args = { command: `cmd-${i}` };
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });

    it("different tools with same args are tracked independently", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { filePath: "/foo" };

      // 4 calls each for read and write — neither reaches threshold alone
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("read", "s-1", args), makeOutput(args));
        hook["tool.execute.before"](makeInput("write", "s-1", args), makeOutput(args));
      }

      // 5th read call triggers
      expect(() =>
        hook["tool.execute.before"](makeInput("read", "s-1", args), makeOutput(args))
      ).toThrow();

      // 5th write call also triggers
      expect(() =>
        hook["tool.execute.before"](makeInput("write", "s-1", args), makeOutput(args))
      ).toThrow();
    });

    it("handles nested object args with stable hashing", () => {
      const hook = createCircuitBreakerHook({ threshold: 3 });

      hook["tool.execute.before"](
        makeInput("bash", "s-1", { nested: { b: 2, a: 1 } }),
        makeOutput({ nested: { b: 2, a: 1 } })
      );
      hook["tool.execute.before"](
        makeInput("bash", "s-1", { nested: { a: 1, b: 2 } }),
        makeOutput({ nested: { a: 1, b: 2 } })
      );

      // 3rd call — same logical args — should trigger
      expect(() =>
        hook["tool.execute.before"](
          makeInput("bash", "s-1", { nested: { a: 1, b: 2 } }),
          makeOutput({ nested: { a: 1, b: 2 } })
        )
      ).toThrow();
    });

    it("handles null and undefined args gracefully", () => {
      const hook = createCircuitBreakerHook({ threshold: 3 });

      // null args
      hook["tool.execute.before"](makeInput("bash", "s-1", null), makeOutput(null));
      hook["tool.execute.before"](makeInput("bash", "s-1", null), makeOutput(null));

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", null), makeOutput(null))
      ).toThrow();
    });

    it("handles empty args", () => {
      const hook = createCircuitBreakerHook({ threshold: 3 });

      hook["tool.execute.before"](makeInput("bash", "s-1"), makeOutput());
      hook["tool.execute.before"](makeInput("bash", "s-1"), makeOutput());

      expect(() => hook["tool.execute.before"](makeInput("bash", "s-1"), makeOutput())).toThrow();
    });
  });

  describe("action configuration", () => {
    it("throws on trigger when action is 'abort' (default)", () => {
      const hook = createCircuitBreakerHook({ threshold: 5, action: "abort" });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();
    });

    it("does not throw on trigger when action is 'warn'", () => {
      const hook = createCircuitBreakerHook({ threshold: 5, action: "warn" });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).not.toThrow();
    });

    it("logs a warning when action is 'warn' and threshold reached", () => {
      const warnSpy = spyOn(console, "warn").mockImplementation(() => {});
      try {
        const hook = createCircuitBreakerHook({ threshold: 3, action: "warn" });
        const args = { command: "ls" };

        for (let i = 0; i < 2; i++) {
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
        }

        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));

        expect(warnSpy).toHaveBeenCalledTimes(1);
        expect(warnSpy.mock.calls[0][0]).toMatch(/repetitive tool use detected/i);
      } finally {
        warnSpy.mockRestore();
      }
    });

    it("includes tool name and session ID in error message", () => {
      const hook = createCircuitBreakerHook({ threshold: 2 });
      const args = { command: "ls" };

      hook["tool.execute.before"](makeInput("bash", "my-session", args), makeOutput(args));

      try {
        hook["tool.execute.before"](makeInput("bash", "my-session", args), makeOutput(args));
        // Should not reach here
        expect(true).toBe(false);
      } catch (e) {
        const message = (e as Error).message;
        expect(message).toContain("bash");
        expect(message).toContain("my-session");
        expect(message).toContain("2 times");
      }
    });
  });

  describe("session cleanup", () => {
    it("clears tracking on session.deleted event", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Fill to 4 calls
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      // Delete session
      await hook.event({
        event: { type: "session.deleted", properties: { sessionID: "s-1" } },
      });

      // Counter reset — 4 more calls should not trigger
      for (let i = 0; i < 4; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });

    it("ignores session.deleted for unknown session without error", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });

      await expect(
        hook.event({ event: { type: "session.deleted", properties: { sessionID: "unknown" } } })
      ).resolves.toBeUndefined();
    });

    it("does not affect other sessions on session.deleted", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Fill both sessions to 4 calls
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args));
      }

      // Delete s-1
      await hook.event({
        event: { type: "session.deleted", properties: { sessionID: "s-1" } },
      });

      // s-2 still at 4 calls — 5th should trigger
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args))
      ).toThrow();

      // s-1 was reset — should not trigger
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).not.toThrow();
    });

    it("ignores non-session.deleted events", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Fill to 4 calls
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      // Non-session.deleted event should not clear tracking
      await hook.event({
        event: { type: "session.created", properties: { sessionID: "s-1" } },
      });

      // 5th call should still trigger
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();
    });

    it("handles session.deleted with info.id format", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      // Use info.id format (alternative shape handled by resolveSessionID)
      await hook.event({
        event: { type: "session.deleted", properties: { info: { id: "s-1" } } },
      });

      // Counter should be reset
      for (let i = 0; i < 4; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });
  });

  describe("disabled state", () => {
    it("never triggers when enabled is false, even after many identical calls", () => {
      const hook = createCircuitBreakerHook({ enabled: false, threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 10; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });

    it("is enabled by default when enabled is not specified", () => {
      const hook = createCircuitBreakerHook({ threshold: 3 });
      const args = { command: "ls" };

      for (let i = 0; i < 2; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();
    });
  });

  describe("edge cases", () => {
    it("handles missing sessionID gracefully (no-op)", () => {
      const hook = createCircuitBreakerHook({ threshold: 2 });
      const args = { command: "ls" };

      // Calls without sessionID should be silently ignored
      for (let i = 0; i < 10; i++) {
        expect(() =>
          hook["tool.execute.before"]({ tool: "bash", callID: "c-1", args }, makeOutput(args))
        ).not.toThrow();
      }
    });

    it("uses output.args for hashing (consistent with tool.execute.before pattern)", () => {
      const hook = createCircuitBreakerHook({ threshold: 3 });

      // Input args differ but output.args are the same — should count as same
      hook["tool.execute.before"](
        makeInput("read", "s-1", { different: "input" }),
        makeOutput({ path: "/foo" })
      );
      hook["tool.execute.before"](
        makeInput("read", "s-1", { also: "different" }),
        makeOutput({ path: "/foo" })
      );

      expect(() =>
        hook["tool.execute.before"](
          makeInput("read", "s-1", { yet: "another" }),
          makeOutput({ path: "/foo" })
        )
      ).toThrow();
    });
  });
});

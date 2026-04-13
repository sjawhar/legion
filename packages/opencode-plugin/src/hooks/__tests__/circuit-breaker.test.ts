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
    it("does not trigger on 4 identical calls (below threshold)", () => {
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
  });

  describe("arg normalization", () => {
    it("treats args with different key order as identical (stable hash)", () => {
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

      // 4 calls with /foo
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](
          makeInput("read", "s-1", { filePath: `/foo${i}` }),
          makeOutput({ filePath: `/foo${i}` })
        );
      }

      // 5th call with a different path — should NOT trigger
      expect(() =>
        hook["tool.execute.before"](
          makeInput("read", "s-1", { filePath: "/bar" }),
          makeOutput({ filePath: "/bar" })
        )
      ).not.toThrow();
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

      // Neither session should trigger
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).not.toThrow(); // s-1 now at 4, still below 5

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args))
      ).not.toThrow(); // s-2 now at 3, still below 5
    });

    it("session 1 triggering does not affect session 2", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Trigger s-1
      for (let i = 0; i < 5; i++) {
        try {
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
        } catch {
          // expected on 5th
        }
      }

      // s-2 should still be clean
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args))
      ).not.toThrow();
    });
  });

  describe("session cleanup", () => {
    it("clears tracking on session.deleted event", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Build up 4 calls
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      // Delete the session
      await hook.event({
        event: { type: "session.deleted", properties: { sessionID: "s-1" } },
      });

      // Counter should be reset — 4 more calls should not trigger
      for (let i = 0; i < 4; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });

    it("does not clear other sessions on session.deleted", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      // Build up 4 calls in s-1 and s-2
      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args));
      }

      // Delete only s-1
      await hook.event({
        event: { type: "session.deleted", properties: { sessionID: "s-1" } },
      });

      // s-2 should still have its count — 5th call should trigger
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-2", args), makeOutput(args))
      ).toThrow();
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
      const warnSpy = spyOn(console, "warn").mockImplementation(() => {});

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).not.toThrow();

      expect(warnSpy).toHaveBeenCalledWith(expect.stringMatching(/repetitive tool use detected/i));
      warnSpy.mockRestore();
    });
  });

  describe("disabled state", () => {
    it("does not trigger when enabled is false", () => {
      const hook = createCircuitBreakerHook({ enabled: false, threshold: 2 });
      const args = { command: "ls" };

      for (let i = 0; i < 10; i++) {
        expect(() =>
          hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
        ).not.toThrow();
      }
    });
  });

  describe("edge cases", () => {
    it("handles missing sessionID gracefully (no tracking, no throw)", () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 10; i++) {
        expect(() =>
          hook["tool.execute.before"](
            { tool: "bash", sessionID: undefined as unknown as string, callID: "c-1", args },
            makeOutput(args)
          )
        ).not.toThrow();
      }
    });

    it("ignores non-session.deleted events", async () => {
      const hook = createCircuitBreakerHook({ threshold: 5 });
      const args = { command: "ls" };

      for (let i = 0; i < 4; i++) {
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args));
      }

      // Fire a different event type
      await hook.event({ event: { type: "session.created", properties: { sessionID: "s-1" } } });

      // Count should still be 4 — 5th call triggers
      expect(() =>
        hook["tool.execute.before"](makeInput("bash", "s-1", args), makeOutput(args))
      ).toThrow();
    });
  });
});

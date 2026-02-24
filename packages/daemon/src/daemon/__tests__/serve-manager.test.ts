import { afterEach, describe, expect, it, mock } from "bun:test";
import {
  createSession,
  createWorkerClient,
  healthCheck,
  spawnSharedServe,
  stopServe,
  waitForHealthy,
} from "../serve-manager";

describe("serve-manager", () => {
  const originalSpawn = Bun.spawn;
  const originalFetch = globalThis.fetch;
  const originalKill = process.kill;

  afterEach(() => {
    Bun.spawn = originalSpawn;
    globalThis.fetch = originalFetch;
    process.kill = originalKill;
  });

  describe("spawnSharedServe", () => {
    it("spawns opencode serve on the given port", async () => {
      const spawnArgs = { cmd: [] as string[], options: {} as Record<string, unknown> };
      Bun.spawn = ((cmd: string[], options: Record<string, unknown>) => {
        spawnArgs.cmd = cmd;
        spawnArgs.options = options;
        return { pid: 4242 };
      }) as typeof Bun.spawn;

      const result = await spawnSharedServe({
        port: 13381,
        workspace: "/tmp/legion",
      });

      expect(result.port).toBe(13381);
      expect(result.pid).toBe(4242);
      expect(result.status).toBe("starting");
      expect(spawnArgs.cmd).toEqual(["opencode", "serve", "--port", "13381"]);
      expect(spawnArgs.options.cwd).toBe("/tmp/legion");
      expect((spawnArgs.options.env as Record<string, string>).SUPERPOWERS_SKIP_BOOTSTRAP).toBe(
        "1"
      );
    });

    it("strips OPENCODE_PERMISSION from environment", async () => {
      const spawnArgs = { options: {} as Record<string, unknown> };
      Bun.spawn = ((_: string[], options: Record<string, unknown>) => {
        spawnArgs.options = options;
        return { pid: 4243 };
      }) as typeof Bun.spawn;

      const origPermission = process.env.OPENCODE_PERMISSION;
      process.env.OPENCODE_PERMISSION = '{"skill":{}}';
      try {
        await spawnSharedServe({ port: 13381, workspace: "/tmp" });
        expect(
          (spawnArgs.options.env as Record<string, string>).OPENCODE_PERMISSION
        ).toBeUndefined();
      } finally {
        if (origPermission !== undefined) {
          process.env.OPENCODE_PERMISSION = origPermission;
        } else {
          delete process.env.OPENCODE_PERMISSION;
        }
      }
    });
  });

  describe("waitForHealthy", () => {
    it("resolves when health check passes", async () => {
      let calls = 0;
      globalThis.fetch = (async () => {
        calls += 1;
        return new Response(JSON.stringify({ healthy: true }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }) as unknown as typeof fetch;

      await waitForHealthy(13381, 5, 10);
      expect(calls).toBe(1);
    });

    it("retries until healthy", async () => {
      let calls = 0;
      globalThis.fetch = (async () => {
        calls += 1;
        if (calls < 3) {
          throw new Error("not ready");
        }
        return new Response(JSON.stringify({ healthy: true }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }) as unknown as typeof fetch;

      await waitForHealthy(13381, 5, 10);
      expect(calls).toBe(3);
    });

    it("throws after max retries", async () => {
      globalThis.fetch = (async () => {
        throw new Error("not ready");
      }) as unknown as typeof fetch;

      await expect(waitForHealthy(13381, 3, 10)).rejects.toThrow(
        "did not become healthy after 3 retries"
      );
    });
  });

  describe("createSession", () => {
    it("creates session with correct headers", async () => {
      const captured: {
        url: string;
        headers: Record<string, string>;
        body: Record<string, unknown> | null;
      } = {
        url: "",
        headers: {},
        body: null,
      };
      globalThis.fetch = (async (input: string, init?: RequestInit) => {
        captured.url = input;
        if (init?.headers && typeof init.headers === "object") {
          for (const [k, v] of Object.entries(init.headers)) {
            captured.headers[k.toLowerCase()] = String(v);
          }
        }
        captured.body = JSON.parse(init?.body as string) as Record<string, unknown>;
        return new Response(JSON.stringify({ id: "ses_test" }), { status: 200 });
      }) as unknown as typeof fetch;

      const createdSessionId = await createSession(13381, "ses_test123", "/home/user/workspace");

      expect(captured.url).toBe("http://127.0.0.1:13381/session");
      expect(captured.headers["x-opencode-directory"]).toBe(
        encodeURIComponent("/home/user/workspace")
      );
      expect(captured.body?.id).toBe("ses_test123");
      expect(createdSessionId).toBe("ses_test");
    });

    it("returns requested ID on 409 DuplicateIDError", async () => {
      globalThis.fetch = (async () => {
        return new Response(JSON.stringify({ name: "DuplicateIDError" }), { status: 409 });
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_existing", "/tmp")).resolves.toBe("ses_existing");
    });

    it("returns session ID from response body on success", async () => {
      globalThis.fetch = (async () => {
        return new Response(JSON.stringify({ id: "ses_actual" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_requested", "/tmp")).resolves.toBe("ses_actual");
    });

    it("throws when response body missing id field", async () => {
      globalThis.fetch = (async () => {
        return new Response(JSON.stringify({}), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_requested", "/tmp")).rejects.toThrow(
        "createSession: response body missing or invalid id field"
      );
    });

    it("warns when returned ID differs from requested", async () => {
      globalThis.fetch = (async () => {
        return new Response(JSON.stringify({ id: "ses_actual" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }) as unknown as typeof fetch;
      const warn = mock(() => {});
      const originalWarn = console.warn;
      console.warn = warn;

      try {
        await expect(createSession(13381, "ses_requested", "/tmp")).resolves.toBe("ses_actual");
      } finally {
        console.warn = originalWarn;
      }

      expect(warn).toHaveBeenCalledWith(
        "createSession: session ID mismatch: requested=ses_requested actual=ses_actual"
      );
    });

    it("throws on other errors", async () => {
      globalThis.fetch = (async () => {
        return new Response(JSON.stringify({ error: "internal" }), { status: 500 });
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_fail", "/tmp")).rejects.toThrow(
        "Failed to create session"
      );
    });

    it("throws descriptive error on non-JSON 2xx response", async () => {
      globalThis.fetch = (async () => {
        return new Response("OK", { status: 200 });
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_test", "/tmp")).rejects.toThrow(/not valid JSON.*200/);
    });

    it("409 with non-DuplicateIDError body throws", async () => {
      globalThis.fetch = (async () => {
        return new Response(JSON.stringify({ name: "ConflictError" }), { status: 409 });
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_test", "/tmp")).rejects.toThrow(
        "Failed to create session"
      );
    });

    it("409 with non-JSON body throws", async () => {
      globalThis.fetch = (async () => {
        return new Response("Conflict", { status: 409 });
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_test", "/tmp")).rejects.toThrow(
        "Failed to create session"
      );
    });

    it("non-409 with DuplicateIDError body throws", async () => {
      globalThis.fetch = (async () => {
        return new Response(JSON.stringify({ name: "DuplicateIDError" }), { status: 500 });
      }) as unknown as typeof fetch;

      await expect(createSession(13381, "ses_test", "/tmp")).rejects.toThrow(
        "Failed to create session"
      );
    });
  });

  describe("stopServe", () => {
    it("disposes and returns when process exits", async () => {
      const calls = { disposeUrl: "", signals: [] as (number | undefined | NodeJS.Signals)[] };
      globalThis.fetch = (async (input: string) => {
        calls.disposeUrl = input;
        return new Response(null, { status: 200 });
      }) as unknown as typeof fetch;

      process.kill = ((_: number, signal?: NodeJS.Signals) => {
        calls.signals.push(signal);
        const err = new Error("ESRCH") as NodeJS.ErrnoException;
        err.code = "ESRCH";
        throw err;
      }) as typeof process.kill;

      await stopServe(13381, 4242, 50, 10, 100);

      expect(calls.disposeUrl).toBe("http://127.0.0.1:13381/global/dispose");
      expect(calls.signals).toEqual([0]);
    });

    it("sends SIGKILL when process lingers after dispose", async () => {
      const calls = { sigkill: false, signalChecks: 0 };
      globalThis.fetch = (async () =>
        new Response(null, { status: 200 })) as unknown as typeof fetch;

      process.kill = ((_: number, signal?: NodeJS.Signals) => {
        if (signal === "SIGKILL") {
          calls.sigkill = true;
          return true;
        }
        calls.signalChecks += 1;
        return true;
      }) as typeof process.kill;

      await stopServe(13381, 4242, 50, 10, 100);

      expect(calls.signalChecks).toBeGreaterThan(0);
      expect(calls.sigkill).toBe(true);
    });
  });

  describe("healthCheck", () => {
    it("returns true when healthy", async () => {
      globalThis.fetch = (async (url: string) => {
        expect(url).toBe("http://127.0.0.1:15000/global/health");
        return new Response(JSON.stringify({ healthy: true }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }) as unknown as typeof fetch;

      expect(await healthCheck(15000, 500)).toBe(true);
    });

    it("returns false on error", async () => {
      globalThis.fetch = (async () => {
        throw new Error("boom");
      }) as unknown as typeof fetch;

      expect(await healthCheck(15001, 500)).toBe(false);
    });

    it("returns false when response body does not match schema", async () => {
      globalThis.fetch = (async () => {
        return new Response(JSON.stringify({ healthy: "true" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }) as unknown as typeof fetch;

      expect(await healthCheck(15002, 500)).toBe(false);
    });
  });

  describe("createWorkerClient", () => {
    it("creates SDK client with correct config", () => {
      const client = createWorkerClient(13381, "/home/user/workspace");
      expect(client).toBeDefined();
      expect(client.session).toBeDefined();
    });
  });
});

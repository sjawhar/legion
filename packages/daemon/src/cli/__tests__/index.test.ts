import { afterEach, beforeEach, describe, expect, it, mock, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  attachCommand,
  cmdDispatch,
  cmdPrompt,
  cmdResetCrashes,
  dispatchCommand,
  getDaemonPort,
  loadTeamsCache,
  promptCommand,
  resetCrashesCommand,
  startCommand,
  statusCommand,
  stopCommand,
  teamsCommand,
} from "../index";

async function resolveArgs(command: { args?: unknown }): Promise<Record<string, unknown>> {
  const { args } = command;
  if (!args) {
    throw new Error("Command has no args");
  }
  if (typeof args === "function") {
    return (await args()) as Record<string, unknown>;
  }
  return args as Record<string, unknown>;
}

describe("citty command definitions", () => {
  test("start command args are defined", async () => {
    const args = await resolveArgs(startCommand);
    const team = args.team as { type: string; required?: boolean };
    const workspace = args.workspace as { type: string; alias?: string; default?: string };
    const stateDir = args["state-dir"] as { type: string };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(workspace.type).toBe("string");
    expect(workspace.alias).toBe("w");
    expect(workspace.default).toBe(process.cwd());
    expect(stateDir.type).toBe("string");
  });

  test("stop command args are defined", async () => {
    const args = await resolveArgs(stopCommand);
    const team = args.team as { type: string; required?: boolean };
    const stateDir = args["state-dir"] as { type: string };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(stateDir.type).toBe("string");
  });

  test("status command args are defined", async () => {
    const args = await resolveArgs(statusCommand);
    const team = args.team as { type: string; required?: boolean };
    const stateDir = args["state-dir"] as { type: string };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(stateDir.type).toBe("string");
  });

  test("attach command args are defined", async () => {
    const args = await resolveArgs(attachCommand);
    const team = args.team as { type: string; required?: boolean };
    const issue = args.issue as { type: string; required?: boolean };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
  });

  test("teams command args are defined", async () => {
    const args = await resolveArgs(teamsCommand);
    const all = args.all as { type: string; default?: boolean };
    expect(all.type).toBe("boolean");
    expect(all.default).toBe(false);
  });

  test("dispatch command args are defined", async () => {
    const args = await resolveArgs(dispatchCommand);
    const issue = args.issue as { type: string; required?: boolean };
    const mode = args.mode as { type: string; required?: boolean };
    const prompt = args.prompt as { type: string };
    const workspace = args.workspace as { type: string; alias?: string };
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
    expect(mode.type).toBe("positional");
    expect(mode.required).toBe(true);
    expect(prompt.type).toBe("string");
    expect(workspace.type).toBe("string");
    expect(workspace.alias).toBe("w");
  });

  test("prompt command args are defined", async () => {
    const args = await resolveArgs(promptCommand);
    const issue = args.issue as { type: string; required?: boolean };
    const prompt = args.prompt as { type: string; required?: boolean };
    const mode = args.mode as { type: string };
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
    expect(prompt.type).toBe("positional");
    expect(prompt.required).toBe(true);
    expect(mode.type).toBe("string");
  });

  test("reset-crashes command args are defined", async () => {
    const args = await resolveArgs(resetCrashesCommand);
    const issue = args.issue as { type: string; required?: boolean };
    const mode = args.mode as { type: string; required?: boolean };
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
    expect(mode.type).toBe("positional");
    expect(mode.required).toBe(true);
  });
});

describe("cmdDispatch", () => {
  const originalFetch = globalThis.fetch;
  const originalSpawnSync = Bun.spawnSync;
  const originalExit = process.exit;
  const originalLog = console.log;
  const originalError = console.error;
  const originalWarn = console.warn;

  beforeEach(() => {
    console.log = mock(() => {}) as unknown as typeof console.log;
    console.error = mock(() => {}) as unknown as typeof console.error;
    console.warn = mock(() => {}) as unknown as typeof console.warn;
    Bun.spawnSync = mock(() => ({
      exitCode: 0,
      stdout: Buffer.from(""),
      stderr: Buffer.from(""),
    })) as unknown as typeof Bun.spawnSync;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    Bun.spawnSync = originalSpawnSync;
    process.exit = originalExit;
    console.log = originalLog;
    console.error = originalError;
    console.warn = originalWarn;
  });

  it("dispatches worker via daemon API and sends initial prompt", async () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-dispatch-"));
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(
          new Response(JSON.stringify({ status: "ok" }), {
            status: 200,
            headers: { "content-type": "application/json" },
          })
        );
      }
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(JSON.stringify({ id: "leg-42-implement", port: 18000, sessionId: "s-1" }), {
            status: 200,
            headers: { "content-type": "application/json" },
          })
        );
      }
      if (url.endsWith("/session/s-1/prompt_async")) {
        return Promise.resolve(new Response("{}", { status: 200 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13371 });

    expect(fetchMock.mock.calls.length).toBe(3);
    const postCall = fetchMock.mock.calls[1] as unknown as [string | URL, RequestInit | undefined];
    const postUrl = postCall[0].toString();
    const postInit = postCall[1] as RequestInit;
    expect(postUrl).toBe("http://127.0.0.1:13371/workers");
    expect(postInit.method).toBe("POST");
    const body = JSON.parse(postInit.body as string) as {
      issueId: string;
      mode: string;
      workspace: string;
    };
    expect(body).toEqual({ issueId: "LEG-42", mode: "implement", workspace: workspacePath });

    const promptCall = fetchMock.mock.calls[2] as unknown as [
      string | URL,
      RequestInit | undefined,
    ];
    const promptUrl = promptCall[0].toString();
    const promptInit = promptCall[1] as RequestInit;
    expect(promptUrl).toBe("http://127.0.0.1:18000/session/s-1/prompt_async");
    expect(JSON.parse(promptInit.body as string)).toEqual({
      parts: [{ type: "text", text: "/legion-worker implement mode for LEG-42" }],
    });
  });

  it("derives workspace path from legionDir and issue identifier", async () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-dispatch-"));
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.resolve(
        new Response(JSON.stringify({ id: "leg-42-implement", port: 18000, sessionId: "s-2" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        })
      );
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13372 });

    const postCall = fetchMock.mock.calls[1] as unknown as [string | URL, RequestInit | undefined];
    const postInit = postCall[1] as RequestInit;
    const body = JSON.parse(postInit.body as string) as {
      issueId: string;
      mode: string;
      workspace: string;
    };
    expect(body.workspace).toBe(workspacePath);
  });

  it("fails gracefully when daemon is not running", async () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-dispatch-"));
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = mock(() => {
      throw new Error("ECONNREFUSED");
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const exitMock = mock((code?: number) => {
      throw new Error(`process.exit:${code ?? 0}`);
    });
    process.exit = exitMock as unknown as typeof process.exit;

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13373 })
    ).rejects.toThrow("process.exit:1");
  });

  it("reports 409 when worker already exists", async () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-dispatch-"));
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.resolve(
        new Response(
          JSON.stringify({
            error: "Worker already exists",
            id: "leg-42-implement",
            port: 18001,
            sessionId: "s-3",
          }),
          { status: 409, headers: { "content-type": "application/json" } }
        )
      );
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13374 });

    expect(fetchMock.mock.calls.length).toBe(2);
  });

  it("reports 429 when crash limit exceeded", async () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-dispatch-"));
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.resolve(
        new Response(
          JSON.stringify({
            error: "Crash limit exceeded",
            id: "leg-42-implement",
            crashCount: 3,
            message: "Too many crashes",
          }),
          { status: 429, headers: { "content-type": "application/json" } }
        )
      );
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const exitMock = mock((code?: number) => {
      throw new Error(`process.exit:${code ?? 0}`);
    });
    process.exit = exitMock as unknown as typeof process.exit;

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13375 })
    ).rejects.toThrow("process.exit:1");
  });

  it("creates jj workspace when directory does not exist", async () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-dispatch-"));
    const legionDir = path.join(tempDir, "legion");
    fs.mkdirSync(legionDir);
    // Do NOT create workspacePath — force jj workspace creation path

    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.resolve(
        new Response(JSON.stringify({ id: "leg-42-implement", port: 18000, sessionId: "s-5" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        })
      );
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13376 });

    const spawnCalls = (Bun.spawnSync as ReturnType<typeof mock>).mock.calls;
    expect(spawnCalls.length).toBe(1);
    const args = spawnCalls[0] as unknown as [string[], Record<string, unknown>];
    expect(args[0]).toEqual([
      "jj",
      "workspace",
      "add",
      path.join(tempDir, "leg-42"),
      "--name",
      "leg-42",
      "-R",
      legionDir,
    ]);
  });

  it("warns but succeeds when prompt delivery fails", async () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-dispatch-"));
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(JSON.stringify({ id: "leg-42-implement", port: 18000, sessionId: "s-6" }), {
            status: 200,
            headers: { "content-type": "application/json" },
          })
        );
      }
      if (url.includes("/prompt_async")) {
        return Promise.reject(new Error("Connection refused"));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13377 });

    const warnCalls = (console.warn as ReturnType<typeof mock>).mock.calls.flat();
    expect(warnCalls).toContain("Worker spawned but prompt delivery failed. Send manually:");
  });
});

describe("cmdPrompt", () => {
  const originalFetch = globalThis.fetch;
  const originalExit = process.exit;
  const originalLog = console.log;
  const originalError = console.error;

  beforeEach(() => {
    console.log = mock(() => {}) as unknown as typeof console.log;
    console.error = mock(() => {}) as unknown as typeof console.error;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    process.exit = originalExit;
    console.log = originalLog;
    console.error = originalError;
  });

  it("sends prompt to existing worker by issue identifier", async () => {
    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(
            JSON.stringify([
              { id: "leg-42-implement", port: 19000, sessionId: "s-42", status: "running" },
            ]),
            { status: 200, headers: { "content-type": "application/json" } }
          )
        );
      }
      if (url.endsWith("/session/s-42/prompt_async")) {
        return Promise.resolve(new Response("{}", { status: 200 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await cmdPrompt("LEG-42", "Check the Linear comments", { daemonPort: 13376 });

    expect(fetchMock.mock.calls.length).toBe(2);
    const secondCall = fetchMock.mock.calls[1] as unknown as [
      string | URL,
      RequestInit | undefined,
    ];
    const secondUrl = secondCall[0].toString();
    const secondInit = secondCall[1] as RequestInit;
    expect(secondUrl).toBe("http://127.0.0.1:19000/session/s-42/prompt_async");
    expect(JSON.parse(secondInit.body as string)).toEqual({
      parts: [{ type: "text", text: "Check the Linear comments" }],
    });
  });

  it("resolves ambiguous issue with --mode flag", async () => {
    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(
            JSON.stringify([
              { id: "leg-42-architect", port: 19001, sessionId: "s-1", status: "running" },
              { id: "leg-42-implement", port: 19002, sessionId: "s-2", status: "running" },
            ]),
            { status: 200, headers: { "content-type": "application/json" } }
          )
        );
      }
      if (url.endsWith("/session/s-2/prompt_async")) {
        return Promise.resolve(new Response("{}", { status: 200 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await cmdPrompt("LEG-42", "Check the Linear comments", {
      daemonPort: 13377,
      mode: "implement",
    });

    const secondCall = fetchMock.mock.calls[1] as unknown as [
      string | URL,
      RequestInit | undefined,
    ];
    expect(secondCall[0].toString()).toBe("http://127.0.0.1:19002/session/s-2/prompt_async");
  });

  it("fails when no worker found", async () => {
    const fetchMock = mock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(
            JSON.stringify([
              { id: "leg-99-implement", port: 19003, sessionId: "s-3", status: "running" },
              { id: "leg-42-implement", port: 19004, sessionId: "s-4", status: "stopped" },
            ]),
            { status: 200, headers: { "content-type": "application/json" } }
          )
        );
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const exitMock = mock((code?: number) => {
      throw new Error(`process.exit:${code ?? 0}`);
    });
    process.exit = exitMock as unknown as typeof process.exit;

    let caught: Error | null = null;
    try {
      await cmdPrompt("LEG-42", "Check the Linear comments", { daemonPort: 13378 });
    } catch (error) {
      caught = error as Error;
    }
    expect(caught?.message).toBe("process.exit:1");
    const errorCalls = (console.error as ReturnType<typeof mock>).mock.calls;
    expect(errorCalls[0]?.[0]).toContain("No active worker found for: LEG-42");
    const logCalls = (console.log as ReturnType<typeof mock>).mock.calls;
    expect(logCalls.flat()).toContain("\nActive workers:");
    expect(logCalls.flat()).toContain("  - leg-99-implement");
  });
});

describe("cmdResetCrashes", () => {
  const originalFetch = globalThis.fetch;
  const originalExit = process.exit;
  const originalLog = console.log;
  const originalError = console.error;

  beforeEach(() => {
    console.log = mock(() => {}) as unknown as typeof console.log;
    console.error = mock(() => {}) as unknown as typeof console.error;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    process.exit = originalExit;
    console.log = originalLog;
    console.error = originalError;
  });

  it("clears crash history for a worker", async () => {
    const fetchMock = mock(() => Promise.resolve(new Response("{}", { status: 200 })));
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    await cmdResetCrashes("LEG-42", "implement", { daemonPort: 13379 });

    expect(fetchMock.mock.calls.length).toBe(1);
    const call = fetchMock.mock.calls[0] as unknown as [string | URL, RequestInit | undefined];
    expect(call[0].toString()).toBe("http://127.0.0.1:13379/workers/leg-42-implement/crashes");
    expect(call[1]?.method).toBe("DELETE");
    const logCalls = (console.log as ReturnType<typeof mock>).mock.calls.flat();
    expect(logCalls).toContain("Crash history cleared for leg-42-implement");
    expect(logCalls).toContain("You can now dispatch: legion dispatch LEG-42 implement");
  });

  it("fails when daemon is not running", async () => {
    const fetchMock = mock(() => {
      throw new Error("ECONNREFUSED");
    });
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const exitMock = mock((code?: number) => {
      throw new Error(`process.exit:${code ?? 0}`);
    });
    process.exit = exitMock as unknown as typeof process.exit;

    return expect(cmdResetCrashes("LEG-42", "implement", { daemonPort: 13380 })).rejects.toThrow(
      "process.exit:1"
    );
  });

  it("exits with error on non-200 response", async () => {
    const fetchMock = mock(() =>
      Promise.resolve(new Response(JSON.stringify({ error: "not_found" }), { status: 404 }))
    );
    globalThis.fetch = fetchMock as unknown as typeof fetch;

    const exitMock = mock((code?: number) => {
      throw new Error(`process.exit:${code ?? 0}`);
    });
    process.exit = exitMock as unknown as typeof process.exit;

    await expect(cmdResetCrashes("LEG-42", "implement", { daemonPort: 13381 })).rejects.toThrow(
      "process.exit:1"
    );

    const errorCalls = (console.error as ReturnType<typeof mock>).mock.calls.flat();
    expect(errorCalls).toContain("Failed to reset crashes: 404");
  });
});

describe("getDaemonPort", () => {
  test("returns default port when env unset", () => {
    expect(getDaemonPort({} as NodeJS.ProcessEnv)).toBe(13370);
  });

  test("returns env port when valid", () => {
    expect(getDaemonPort({ LEGION_DAEMON_PORT: "14400" } as NodeJS.ProcessEnv)).toBe(14400);
  });

  test("falls back when env port invalid", () => {
    expect(getDaemonPort({ LEGION_DAEMON_PORT: "not-a-number" } as NodeJS.ProcessEnv)).toBe(13370);
  });
});

describe("loadTeamsCache", () => {
  test("returns null when cache missing", () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cache-"));
    expect(loadTeamsCache(tempDir)).toBeNull();
  });

  test("loads teams cache from disk", () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cache-"));
    const cacheFile = path.join(tempDir, "teams.json");
    const payload = {
      LEG: { id: "uuid-123", name: "Legion" },
    };
    fs.writeFileSync(cacheFile, JSON.stringify(payload, null, 2));

    const result = loadTeamsCache(tempDir);
    expect(result).toEqual(payload);
  });
});

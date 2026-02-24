import { afterEach, beforeEach, describe, expect, it, mock, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  attachCommand,
  CliError,
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

interface CommandWithArgs {
  args?: unknown;
}

interface PositionalArg {
  type: string;
  required?: boolean;
}

interface StringArg {
  type: string;
  alias?: string;
  default?: string;
}

interface BooleanArg {
  type: string;
  default?: boolean;
}

interface RunnableCommand {
  run?: unknown;
}

type FetchCall = [string | URL, RequestInit?];
type FetchMock = ReturnType<typeof mock> & typeof fetch;
type SpawnSyncMock = ReturnType<typeof mock> & typeof Bun.spawnSync;
type SpawnSyncCall = Parameters<typeof Bun.spawnSync>;

function installFetchMock(
  impl: (input: string | URL, init?: RequestInit) => Promise<Response>
): FetchMock {
  const fetchMock = Object.assign(mock(impl), {
    preconnect: (() => {}) as typeof fetch.preconnect,
  }) as FetchMock;
  globalThis.fetch = fetchMock;
  return fetchMock;
}

function installSpawnSyncMock(
  impl?: (...args: Parameters<typeof Bun.spawnSync>) => ReturnType<typeof Bun.spawnSync>
): SpawnSyncMock {
  const implementation =
    impl ??
    ((..._args: Parameters<typeof Bun.spawnSync>): ReturnType<typeof Bun.spawnSync> =>
      ({
        exitCode: 0,
        stdout: Buffer.from(""),
        stderr: Buffer.from(""),
        success: true,
        pid: 0,
        resourceUsage: {} as ReturnType<typeof Bun.spawnSync>["resourceUsage"],
      }) as ReturnType<typeof Bun.spawnSync>);
  const spawnSyncMock = mock(implementation) as SpawnSyncMock;
  Bun.spawnSync = spawnSyncMock;
  return spawnSyncMock;
}

async function runCommand(command: unknown, args: Record<string, unknown>): Promise<void> {
  const run = (command as RunnableCommand).run as
    | ((context: {
        args: Record<string, unknown>;
        rawArgs: Record<string, unknown>;
        cmd: unknown;
      }) => Promise<unknown> | unknown)
    | undefined;
  if (!run) {
    throw new Error("Command has no run handler");
  }
  const parsedArgs = { _: [], ...args } as Record<string, unknown>;
  await run({
    args: parsedArgs,
    rawArgs: parsedArgs,
    cmd: command as RunnableCommand,
  });
}

async function resolveArgs(command: CommandWithArgs): Promise<Record<string, unknown>> {
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
    const team = args.team as PositionalArg;
    const workspace = args.workspace as StringArg;
    const stateDir = args["state-dir"] as StringArg;
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(workspace.type).toBe("string");
    expect(workspace.alias).toBe("w");
    expect(workspace.default).toBe(process.cwd());
    expect(stateDir.type).toBe("string");
  });

  test("stop command args are defined", async () => {
    const args = await resolveArgs(stopCommand);
    const team = args.team as PositionalArg;
    const stateDir = args["state-dir"] as StringArg;
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(stateDir.type).toBe("string");
  });

  test("status command args are defined", async () => {
    const args = await resolveArgs(statusCommand);
    const team = args.team as PositionalArg;
    const stateDir = args["state-dir"] as StringArg;
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(stateDir.type).toBe("string");
  });

  test("attach command args are defined", async () => {
    const args = await resolveArgs(attachCommand);
    const team = args.team as PositionalArg;
    const issue = args.issue as PositionalArg;
    expect(team.type).toBe("positional");
    expect(team.required).toBe(true);
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
  });

  test("teams command args are defined", async () => {
    const args = await resolveArgs(teamsCommand);
    const all = args.all as BooleanArg;
    expect(all.type).toBe("boolean");
    expect(all.default).toBe(false);
  });

  test("dispatch command args are defined", async () => {
    const args = await resolveArgs(dispatchCommand);
    const issue = args.issue as PositionalArg;
    const mode = args.mode as PositionalArg;
    const prompt = args.prompt as StringArg;
    const workspace = args.workspace as StringArg;
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
    const issue = args.issue as PositionalArg;
    const prompt = args.prompt as PositionalArg;
    const mode = args.mode as StringArg;
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
    expect(prompt.type).toBe("positional");
    expect(prompt.required).toBe(true);
    expect(mode.type).toBe("string");
  });

  test("reset-crashes command args are defined", async () => {
    const args = await resolveArgs(resetCrashesCommand);
    const issue = args.issue as PositionalArg;
    const mode = args.mode as PositionalArg;
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
    expect(mode.type).toBe("positional");
    expect(mode.required).toBe(true);
  });
});

describe("cmdDispatch", () => {
  const originalFetch = globalThis.fetch;
  const originalSpawnSync = Bun.spawnSync;
  const originalLog = console.log;
  const originalError = console.error;
  const originalWarn = console.warn;
  const tempDirs: string[] = [];

  const createTempDir = () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-dispatch-"));
    tempDirs.push(dir);
    return dir;
  };

  beforeEach(() => {
    console.log = mock(() => {}) as typeof console.log;
    console.error = mock(() => {}) as typeof console.error;
    console.warn = mock(() => {}) as typeof console.warn;
    installSpawnSyncMock();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    Bun.spawnSync = originalSpawnSync;
    console.log = originalLog;
    console.error = originalError;
    console.warn = originalWarn;
    while (tempDirs.length > 0) {
      const dir = tempDirs.pop();
      if (dir && fs.existsSync(dir)) {
        fs.rmSync(dir, { recursive: true, force: true });
      }
    }
  });

  it("dispatches worker via daemon API and sends initial prompt", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = installFetchMock((input: string | URL) => {
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
    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13371 });

    expect(fetchMock.mock.calls.length).toBe(3);
    const [postUrl, postInit] = fetchMock.mock.calls[1] as FetchCall;
    const postInitResolved = postInit as RequestInit;
    expect(postUrl.toString()).toBe("http://127.0.0.1:13371/workers");
    expect(postInitResolved.method).toBe("POST");
    const body = JSON.parse(postInitResolved.body as string) as {
      issueId: string;
      mode: string;
      workspace: string;
    };
    expect(body).toEqual({ issueId: "LEG-42", mode: "implement", workspace: workspacePath });

    const [promptUrl, promptInit] = fetchMock.mock.calls[2] as FetchCall;
    expect(promptUrl.toString()).toBe("http://127.0.0.1:18000/session/s-1/prompt_async");
    expect(JSON.parse((promptInit as RequestInit).body as string)).toEqual({
      parts: [{ type: "text", text: "/legion-worker implement mode for LEG-42" }],
    });
  });

  it("derives workspace path from legionDir and issue identifier", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = installFetchMock((input: string | URL) => {
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
    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13372 });

    const [, postInit] = fetchMock.mock.calls[1] as FetchCall;
    const body = JSON.parse((postInit as RequestInit).body as string) as {
      issueId: string;
      mode: string;
      workspace: string;
    };
    expect(body.workspace).toBe(workspacePath);
  });

  it("fails gracefully when daemon is not running", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    installFetchMock(() => {
      throw new Error("ECONNREFUSED");
    });

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13373 })
    ).rejects.toThrow(CliError);
  });

  it("reports 409 when worker already exists", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    const fetchMock = installFetchMock((input: string | URL) => {
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
    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13374 });

    expect(fetchMock.mock.calls.length).toBe(2);
  });

  it("reports 429 when crash limit exceeded", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    installFetchMock((input: string | URL) => {
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

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13375 })
    ).rejects.toThrow(CliError);
  });

  it("creates jj workspace when directory does not exist", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    fs.mkdirSync(legionDir);
    // Do NOT create workspacePath — force jj workspace creation path

    installFetchMock((input: string | URL) => {
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

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13376, vcs: "jj" });

    const spawnCalls = (Bun.spawnSync as SpawnSyncMock).mock.calls;
    expect(spawnCalls.length).toBe(1);
    const spawnCall = spawnCalls[0] as SpawnSyncCall;
    expect(spawnCall[0]).toEqual([
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

  it("creates git worktree when vcs is git and directory does not exist", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    fs.mkdirSync(legionDir);

    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.resolve(
        new Response(JSON.stringify({ id: "leg-42-implement", port: 18000, sessionId: "s-git" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        })
      );
    });

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13377, vcs: "git" });

    const spawnCalls = (Bun.spawnSync as SpawnSyncMock).mock.calls;
    expect(spawnCalls.length).toBe(1);
    const spawnCall = spawnCalls[0] as SpawnSyncCall;
    expect(spawnCall[0]).toEqual([
      "git",
      "worktree",
      "add",
      "-B",
      "legion/leg-42",
      path.join(tempDir, "leg-42"),
      "origin/main",
    ]);
  });

  it("warns but succeeds when prompt delivery fails", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    installFetchMock((input: string | URL) => {
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

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13377 });

    const warnCalls = (console.warn as ReturnType<typeof mock>).mock.calls.flat();
    expect(warnCalls).toContain("Worker spawned but prompt delivery failed. Send manually:");
  });

  it("rejects invalid issue identifiers", () => {
    return expect(cmdDispatch("LEG 42", "implement", { daemonPort: 13370 })).rejects.toThrow(
      "Invalid issue identifier"
    );
  });

  it("rejects invalid mode identifiers", () => {
    return expect(cmdDispatch("LEG-42", "impl!ment", { daemonPort: 13370 })).rejects.toThrow(
      "Invalid mode"
    );
  });

  it("fails when daemon health check is unhealthy", () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    fs.mkdirSync(legionDir);

    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response("unhealthy", { status: 500 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13382 })
    ).rejects.toThrow("Daemon is not healthy");
  });

  it("fails when jj workspace creation fails", () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    fs.mkdirSync(legionDir);

    installSpawnSyncMock(
      (..._args: Parameters<typeof Bun.spawnSync>): ReturnType<typeof Bun.spawnSync> =>
        ({
          exitCode: 1,
          stdout: Buffer.from(""),
          stderr: Buffer.from("jj failed"),
          success: false,
          pid: 0,
          resourceUsage: {} as ReturnType<typeof Bun.spawnSync>["resourceUsage"],
        }) as ReturnType<typeof Bun.spawnSync>
    );

    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13383 })
    ).rejects.toThrow("Failed to create workspace: jj failed");
  });

  it("fails when daemon worker creation cannot connect", () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      if (url.endsWith("/workers")) {
        throw new Error("ECONNRESET");
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13384 })
    ).rejects.toThrow("Could not connect to daemon");
  });

  it("fails when daemon returns non-JSON response", () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      if (url.endsWith("/workers")) {
        return Promise.resolve(new Response("not-json", { status: 200 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13385 })
    ).rejects.toThrow("Daemon returned non-JSON response (status 200)");
  });

  it("fails when daemon returns non-OK status", () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    const workspacePath = path.join(tempDir, "leg-42");
    fs.mkdirSync(legionDir);
    fs.mkdirSync(workspacePath);

    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(JSON.stringify({ error: "nope" }), {
            status: 500,
            headers: { "content-type": "application/json" },
          })
        );
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(
      cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13386 })
    ).rejects.toThrow('Failed to dispatch: {"error":"nope"}');
  });
});

describe("cmdAttach", () => {
  const originalFetch = globalThis.fetch;
  const originalLog = console.log;
  const originalError = console.error;
  const originalExit = process.exit;
  let exitCode: number | undefined;

  beforeEach(() => {
    console.log = mock(() => {}) as typeof console.log;
    console.error = mock(() => {}) as typeof console.error;
    exitCode = undefined;
    process.exit = mock((code?: number) => {
      exitCode = code;
      return undefined as never;
    }) as typeof process.exit;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    console.log = originalLog;
    console.error = originalError;
    process.exit = originalExit;
  });

  it("fails when daemon cannot be reached", async () => {
    installFetchMock(() => Promise.resolve(new Response("", { status: 500 })));

    try {
      await Promise.resolve(
        runCommand(attachCommand, {
          team: "12345678-1234-1234-1234-123456789abc",
          issue: "LEG-42",
        })
      );
    } catch {}

    const errorCalls = (console.error as ReturnType<typeof mock>).mock.calls.flat();
    expect(errorCalls).toContain("Could not connect to daemon. Is it running?");
    expect(exitCode).toBe(1);
  });

  it("fails when no worker matches issue", async () => {
    installFetchMock(() =>
      Promise.resolve(
        new Response(JSON.stringify([{ id: "leg-1-plan", port: 1234 }]), {
          status: 200,
          headers: { "content-type": "application/json" },
        })
      )
    );

    try {
      await Promise.resolve(
        runCommand(attachCommand, {
          team: "12345678-1234-1234-1234-123456789abc",
          issue: "LEG-42",
        })
      );
    } catch {}

    const errorCalls = (console.error as ReturnType<typeof mock>).mock.calls.flat();
    expect(errorCalls.join("\n")).toContain("No worker found for issue: LEG-42");
    expect(exitCode).toBe(1);
  });

  it("fails when multiple workers match issue", async () => {
    installFetchMock(() =>
      Promise.resolve(
        new Response(
          JSON.stringify([
            { id: "leg-42-architect", port: 2000 },
            { id: "leg-42-implement", port: 2001 },
          ]),
          {
            status: 200,
            headers: { "content-type": "application/json" },
          }
        )
      )
    );

    try {
      await Promise.resolve(
        runCommand(attachCommand, {
          team: "12345678-1234-1234-1234-123456789abc",
          issue: "LEG-42",
        })
      );
    } catch {}

    const errorCalls = (console.error as ReturnType<typeof mock>).mock.calls.flat();
    expect(errorCalls.join("\n")).toContain("Multiple workers found for LEG-42");
    expect(exitCode).toBe(1);
  });

  it("wraps unexpected errors as CliError", async () => {
    installFetchMock(() => {
      throw new Error("boom");
    });

    try {
      await Promise.resolve(
        runCommand(attachCommand, {
          team: "12345678-1234-1234-1234-123456789abc",
          issue: "LEG-42",
        })
      );
    } catch {}

    const errorCalls = (console.error as ReturnType<typeof mock>).mock.calls.flat();
    expect(errorCalls.join("\n")).toContain("Failed to attach: boom");
    expect(exitCode).toBe(1);
  });
});

describe("cmdPrompt", () => {
  const originalFetch = globalThis.fetch;
  const originalLog = console.log;
  const originalError = console.error;

  beforeEach(() => {
    console.log = mock(() => {}) as typeof console.log;
    console.error = mock(() => {}) as typeof console.error;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    console.log = originalLog;
    console.error = originalError;
  });

  it("sends prompt to existing worker by issue identifier", async () => {
    const fetchMock = installFetchMock((input: string | URL) => {
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
    await cmdPrompt("LEG-42", "Check the issue comments", { daemonPort: 13376 });

    expect(fetchMock.mock.calls.length).toBe(2);
    const [secondUrl, secondInit] = fetchMock.mock.calls[1] as FetchCall;
    expect(secondUrl.toString()).toBe("http://127.0.0.1:19000/session/s-42/prompt_async");
    expect(JSON.parse((secondInit as RequestInit).body as string)).toEqual({
      parts: [{ type: "text", text: "Check the issue comments" }],
    });
  });

  it("resolves ambiguous issue with --mode flag", async () => {
    const fetchMock = installFetchMock((input: string | URL) => {
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
    await cmdPrompt("LEG-42", "Check the issue comments", {
      daemonPort: 13377,
      mode: "implement",
    });

    const [secondUrl] = fetchMock.mock.calls[1] as FetchCall;
    expect(secondUrl.toString()).toBe("http://127.0.0.1:19002/session/s-2/prompt_async");
  });

  it("fails when no worker found", async () => {
    installFetchMock((input: string | URL) => {
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

    let caught: CliError | null = null;
    try {
      await cmdPrompt("LEG-42", "Check the issue comments", { daemonPort: 13378 });
    } catch (error) {
      caught = error as CliError;
    }
    expect(caught).toBeInstanceOf(CliError);
    expect(caught?.message).toContain("No active worker found for: LEG-42");
    expect(caught?.message).toContain("leg-99-implement");
  });

  it("rejects invalid issue identifiers", () => {
    return expect(cmdPrompt("LEG 42", "test", { daemonPort: 13390 })).rejects.toThrow(
      "Invalid issue identifier"
    );
  });

  it("fails when workers endpoint returns non-ok", () => {
    installFetchMock(() => Promise.resolve(new Response("", { status: 500 })));

    return expect(cmdPrompt("LEG-42", "test", { daemonPort: 13391 })).rejects.toThrow(
      "Could not connect to daemon."
    );
  });

  it("fails when workers endpoint throws", () => {
    installFetchMock(() => {
      throw new Error("ECONNREFUSED");
    });

    return expect(cmdPrompt("LEG-42", "test", { daemonPort: 13392 })).rejects.toThrow(
      "Could not connect to daemon. Is it running?"
    );
  });

  it("fails when multiple workers match without mode", () => {
    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(
            JSON.stringify([
              { id: "leg-42-architect", port: 2001, sessionId: "s-1", status: "running" },
              { id: "leg-42-implement", port: 2002, sessionId: "s-2", status: "running" },
            ]),
            { status: 200, headers: { "content-type": "application/json" } }
          )
        );
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(cmdPrompt("LEG-42", "test", { daemonPort: 13393 })).rejects.toThrow(
      "Multiple workers found for LEG-42"
    );
  });

  it("fails when worker rejects prompt", () => {
    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(
            JSON.stringify([
              { id: "leg-42-implement", port: 2003, sessionId: "s-3", status: "running" },
            ]),
            { status: 200, headers: { "content-type": "application/json" } }
          )
        );
      }
      if (url.endsWith("/session/s-3/prompt_async")) {
        return Promise.resolve(new Response("", { status: 500 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(cmdPrompt("LEG-42", "test", { daemonPort: 13394 })).rejects.toThrow(
      "Worker rejected prompt (status 500): leg-42-implement"
    );
  });

  it("fails when prompt delivery throws", () => {
    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(
            JSON.stringify([
              { id: "leg-42-implement", port: 2004, sessionId: "s-4", status: "running" },
            ]),
            { status: 200, headers: { "content-type": "application/json" } }
          )
        );
      }
      if (url.endsWith("/session/s-4/prompt_async")) {
        throw new Error("write failed");
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(cmdPrompt("LEG-42", "test", { daemonPort: 13395 })).rejects.toThrow(
      "Failed to send prompt to leg-42-implement (port 2004)"
    );
  });
});

describe("cmdResetCrashes", () => {
  const originalFetch = globalThis.fetch;
  const originalLog = console.log;
  const originalError = console.error;

  beforeEach(() => {
    console.log = mock(() => {}) as typeof console.log;
    console.error = mock(() => {}) as typeof console.error;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    console.log = originalLog;
    console.error = originalError;
  });

  it("clears crash history for a worker", async () => {
    const fetchMock = installFetchMock(() => Promise.resolve(new Response("{}", { status: 200 })));

    await cmdResetCrashes("LEG-42", "implement", { daemonPort: 13379 });

    expect(fetchMock.mock.calls.length).toBe(1);
    const call = fetchMock.mock.calls[0] as FetchCall;
    expect(call[0].toString()).toBe("http://127.0.0.1:13379/workers/leg-42-implement/crashes");
    expect(call[1]?.method).toBe("DELETE");
    const logCalls = (console.log as ReturnType<typeof mock>).mock.calls.flat();
    expect(logCalls).toContain("Crash history cleared for leg-42-implement");
    expect(logCalls).toContain("You can now dispatch: legion dispatch LEG-42 implement");
  });

  it("fails when daemon is not running", async () => {
    installFetchMock(() => {
      throw new Error("ECONNREFUSED");
    });

    return expect(cmdResetCrashes("LEG-42", "implement", { daemonPort: 13380 })).rejects.toThrow(
      CliError
    );
  });

  it("exits with error on non-200 response", async () => {
    installFetchMock(() =>
      Promise.resolve(new Response(JSON.stringify({ error: "not_found" }), { status: 404 }))
    );

    let caught: CliError | null = null;
    try {
      await cmdResetCrashes("LEG-42", "implement", { daemonPort: 13381 });
    } catch (error) {
      caught = error as CliError;
    }
    expect(caught).toBeInstanceOf(CliError);
    expect(caught?.message).toContain("Failed to reset crashes: 404");
  });

  it("rejects invalid issue identifiers", () => {
    return expect(cmdResetCrashes("LEG 42", "implement", { daemonPort: 13396 })).rejects.toThrow(
      "Invalid issue identifier"
    );
  });

  it("rejects invalid mode identifiers", () => {
    return expect(cmdResetCrashes("LEG-42", "impl!ment", { daemonPort: 13397 })).rejects.toThrow(
      "Invalid mode"
    );
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
    try {
      expect(loadTeamsCache(tempDir)).toBeNull();
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
    }
  });

  test("loads teams cache from disk", () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cache-"));
    try {
      const cacheFile = path.join(tempDir, "teams.json");
      const payload = {
        LEG: { id: "uuid-123", name: "Legion" },
      };
      fs.writeFileSync(cacheFile, JSON.stringify(payload, null, 2));

      const result = loadTeamsCache(tempDir);
      expect(result).toEqual(payload);
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
    }
  });
});

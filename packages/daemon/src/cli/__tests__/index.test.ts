import { afterEach, beforeEach, describe, expect, it, mock, test } from "bun:test";
import fs from "node:fs";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import os, { tmpdir } from "node:os";
import path from "node:path";

// Mock child_process spawn to prevent actual process spawning in tests
const spawnCalls: Array<{ cmd: string; args: string[]; opts?: unknown }> = [];
let spawnExitCode = 0;
let spawnPid = 99999;

mock.module("node:child_process", () => ({
  spawn: (cmd: string, args: string[], opts?: unknown) => {
    spawnCalls.push({ cmd, args, opts });
    const listeners: Record<string, ((...a: unknown[]) => void)[]> = {};
    const child = {
      pid: spawnPid,
      unref: () => {},
      on: (event: string, handler: (...a: unknown[]) => void) => {
        if (!listeners[event]) listeners[event] = [];
        listeners[event].push(handler);
        if (event === "exit") {
          handler(spawnExitCode);
        }
        return child;
      },
    };
    return child;
  },
}));

// Mock runGhCommand from the github backend to control gh CLI calls in tests.
// parseIssueIdParts is re-exported as the real implementation.
const ghCommandCalls: string[][] = [];
let ghCommandResponses: Array<string | Error> = [];
function mockRunGhCommand(args: string[]): Promise<string> {
  ghCommandCalls.push(args);
  const response = ghCommandResponses.shift();
  if (response instanceof Error) {
    return Promise.reject(response);
  }
  return Promise.resolve(response ?? "");
}

// Keep the real parseIssueIdParts implementation
const realParseIssueIdParts = (issueId: string) => {
  const parts = issueId.split("-");
  let numberIdx = -1;
  for (let i = parts.length - 1; i >= 0; i--) {
    if (/^\d+$/.test(parts[i])) {
      numberIdx = i;
      break;
    }
  }
  if (numberIdx < 2) {
    throw new Error(`Cannot parse issueId "${issueId}" — expected format: owner-repo-number`);
  }
  const owner = parts[0];
  const repo = parts.slice(1, numberIdx).join("-");
  const number = parts[numberIdx];
  return { owner, repo, number };
};

mock.module("../../state/backends/github", () => ({
  parseIssueIdParts: realParseIssueIdParts,
  runGhCommand: mockRunGhCommand,
}));

import { resolveLegionPaths } from "../../daemon/paths";
import { parseIssueIdParts } from "../../state/backends/github";
import {
  attachCommand,
  CliError,
  cmdDispatch,
  cmdEnlist,
  cmdPrompt,
  cmdResetCrashes,
  cmdRollback,
  cmdStart,
  createRevertCommit,
  discoverConfigPath,
  dispatchCommand,
  enlistCommand,
  findMergedPR,
  getDaemonPort,
  legionsCommand,
  loadLegionsCache,
  parseEnvJson,
  promptCommand,
  resetCrashesCommand,
  rollbackCommand,
  scanOcRegistry,
  startCommand,
  statusCommand,
  stopCommand,
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
  required?: boolean;
}

interface NumberArg {
  type: string;
  alias?: string;
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

function createTempDir(): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), "legion-cli-"));
}

function installFetchMock(
  impl: (input: string | URL, init?: RequestInit) => Promise<Response>
): FetchMock {
  const fetchMock = Object.assign(mock(impl), {
    preconnect: (() => {}) as typeof fetch.preconnect,
  }) as FetchMock;
  globalThis.fetch = fetchMock;
  return fetchMock;
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

describe("scanOcRegistry", () => {
  let tempDir: string;

  beforeEach(async () => {
    tempDir = await mkdtemp(path.join(tmpdir(), "oc-registry-"));
  });

  afterEach(async () => {
    await rm(tempDir, { recursive: true, force: true });
  });

  it("finds matching session entry", async () => {
    const entry = {
      pid: 12345,
      port: 41089,
      dir: "/home/ubuntu/agent-c/google",
      started: "2026-03-14T20:43:57+00:00",
      session: { id: "ses_316beec6dffevTRQ4mUzpuleS6", title: "Test" },
    };
    await writeFile(path.join(tempDir, "test.json"), JSON.stringify(entry));

    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", tempDir);
    expect(result).toEqual({ pid: 12345, dir: "/home/ubuntu/agent-c/google" });
  });

  it("returns null when no match found", async () => {
    const entry = {
      pid: 12345,
      dir: "/tmp",
      session: { id: "ses_other000000000000000000" },
    };
    await writeFile(path.join(tempDir, "test.json"), JSON.stringify(entry));

    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", tempDir);
    expect(result).toBeNull();
  });

  it("returns null when directory does not exist", async () => {
    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", "/nonexistent");
    expect(result).toBeNull();
  });

  it("skips malformed JSON files", async () => {
    await writeFile(path.join(tempDir, "bad.json"), "not json");
    const entry = {
      pid: 99,
      dir: "/good",
      session: { id: "ses_316beec6dffevTRQ4mUzpuleS6" },
    };
    await writeFile(path.join(tempDir, "good.json"), JSON.stringify(entry));

    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", tempDir);
    expect(result).toEqual({ pid: 99, dir: "/good" });
  });

  it("skips entries with missing pid or dir", async () => {
    const entry = {
      session: { id: "ses_316beec6dffevTRQ4mUzpuleS6" },
    };
    await writeFile(path.join(tempDir, "test.json"), JSON.stringify(entry));

    const result = await scanOcRegistry("ses_316beec6dffevTRQ4mUzpuleS6", tempDir);
    expect(result).toBeNull();
  });
});

describe("discoverConfigPath", () => {
  let tempDir: string;

  beforeEach(async () => {
    tempDir = await mkdtemp(path.join(tmpdir(), "legion-discover-config-"));
  });

  afterEach(async () => {
    await rm(tempDir, { recursive: true, force: true });
  });

  it("returns XDG_CONFIG_HOME path when it exists", async () => {
    const cwd = path.join(tempDir, "cwd");
    const homeDir = path.join(tempDir, "home");
    const xdgConfigHome = path.join(tempDir, "xdg");
    const xdgConfigPath = path.join(xdgConfigHome, "legion", "legion.yaml");

    fs.mkdirSync(cwd, { recursive: true });
    fs.mkdirSync(homeDir, { recursive: true });
    fs.mkdirSync(path.dirname(xdgConfigPath), { recursive: true });
    await writeFile(xdgConfigPath, "project: from-xdg\n");

    expect(discoverConfigPath(cwd, { XDG_CONFIG_HOME: xdgConfigHome }, homeDir)).toBe(
      xdgConfigPath
    );
  });

  it("falls back to ~/.config/legion/legion.yaml when XDG is not set", async () => {
    const cwd = path.join(tempDir, "cwd");
    const homeDir = path.join(tempDir, "home");
    const homeConfigPath = path.join(homeDir, ".config", "legion", "legion.yaml");

    fs.mkdirSync(cwd, { recursive: true });
    fs.mkdirSync(path.dirname(homeConfigPath), { recursive: true });
    await writeFile(homeConfigPath, "project: from-home\n");

    expect(discoverConfigPath(cwd, {}, homeDir)).toBe(homeConfigPath);
  });

  it("falls back to ./legion.yaml when XDG and home config do not exist", async () => {
    const cwd = path.join(tempDir, "cwd");
    const homeDir = path.join(tempDir, "home");
    const cwdConfigPath = path.join(cwd, "legion.yaml");

    fs.mkdirSync(cwd, { recursive: true });
    fs.mkdirSync(homeDir, { recursive: true });
    await writeFile(cwdConfigPath, "project: from-cwd\n");

    expect(discoverConfigPath(cwd, {}, homeDir)).toBe(cwdConfigPath);
  });

  it("returns undefined when no config exists anywhere", () => {
    const cwd = path.join(tempDir, "cwd");
    const homeDir = path.join(tempDir, "home");

    fs.mkdirSync(cwd, { recursive: true });
    fs.mkdirSync(homeDir, { recursive: true });

    expect(discoverConfigPath(cwd, {}, homeDir)).toBeUndefined();
  });

  it("prefers XDG_CONFIG_HOME over ~/.config", async () => {
    const cwd = path.join(tempDir, "cwd");
    const homeDir = path.join(tempDir, "home");
    const xdgConfigHome = path.join(tempDir, "xdg");
    const xdgConfigPath = path.join(xdgConfigHome, "legion", "legion.yaml");
    const homeConfigPath = path.join(homeDir, ".config", "legion", "legion.yaml");

    fs.mkdirSync(cwd, { recursive: true });
    fs.mkdirSync(path.dirname(xdgConfigPath), { recursive: true });
    fs.mkdirSync(path.dirname(homeConfigPath), { recursive: true });
    await writeFile(xdgConfigPath, "project: from-xdg\n");
    await writeFile(homeConfigPath, "project: from-home\n");

    expect(discoverConfigPath(cwd, { XDG_CONFIG_HOME: xdgConfigHome }, homeDir)).toBe(
      xdgConfigPath
    );
  });
});

describe("cmdStart config wiring", () => {
  const originalLog = console.log;
  const originalError = console.error;
  const originalCwd = process.cwd();
  const originalHome = process.env.HOME;
  const originalXdgConfigHome = process.env.XDG_CONFIG_HOME;
  const startDaemonCalls: Array<Record<string, unknown>> = [];
  const resolveLegionIdCalls: Array<{ team: string; backend?: string }> = [];
  let resolveLegionIdResult: string | undefined;
  const START_DAEMON_ABORT = "__start-daemon-abort__";

  function readBackendArg(
    opts: string | { cacheDir?: string; backend?: string } | undefined
  ): string | undefined {
    return opts && typeof opts === "object" ? opts.backend : undefined;
  }

  beforeEach(() => {
    startDaemonCalls.length = 0;
    resolveLegionIdCalls.length = 0;
    resolveLegionIdResult = undefined;
    console.log = mock(() => {}) as typeof console.log;
    console.error = mock(() => {}) as typeof console.error;
  });

  afterEach(() => {
    console.log = originalLog;
    console.error = originalError;
    process.chdir(originalCwd);
    if (originalHome === undefined) {
      delete process.env.HOME;
    } else {
      process.env.HOME = originalHome;
    }
    if (originalXdgConfigHome === undefined) {
      delete process.env.XDG_CONFIG_HOME;
    } else {
      process.env.XDG_CONFIG_HOME = originalXdgConfigHome;
    }
  });

  it("auto-discovers config and passes it to resolveDaemonConfig", async () => {
    const tempDir = await mkdtemp(path.join(tmpdir(), "legion-start-autodiscover-"));
    const homeDir = path.join(tempDir, "home");
    const xdgConfigHome = path.join(tempDir, "xdg");
    const configPath = path.join(xdgConfigHome, "legion", "legion.yaml");

    try {
      fs.mkdirSync(homeDir, { recursive: true });
      fs.mkdirSync(path.dirname(configPath), { recursive: true });
      await writeFile(
        configPath,
        ["project: auto/team", "workspace: /tmp/auto-workspace", "port: 15433"].join("\n")
      );
      process.chdir(tempDir);
      process.env.HOME = homeDir;
      process.env.XDG_CONFIG_HOME = xdgConfigHome;

      try {
        await cmdStart(
          undefined,
          { foreground: true },
          {
            startDaemon: async (config) => {
              startDaemonCalls.push(config as unknown as Record<string, unknown>);
              throw new Error(START_DAEMON_ABORT);
            },
            resolveLegionId: async (team, opts) => {
              resolveLegionIdCalls.push({ team, backend: readBackendArg(opts) });
              return resolveLegionIdResult ?? team;
            },
          }
        );
        throw new Error("Expected cmdStart to reject");
      } catch (error) {
        expect(error).toBeInstanceOf(Error);
        expect((error as Error).message).toContain(START_DAEMON_ABORT);
      }

      expect(resolveLegionIdCalls).toHaveLength(0);
      expect(startDaemonCalls).toHaveLength(1);
      expect(startDaemonCalls[0]).toEqual(
        expect.objectContaining({
          legionId: "auto/team",
          legionDir: "/tmp/auto-workspace",
          daemonPort: 15433,
          daemonPortExplicit: true,
        })
      );
      expect(console.log).toHaveBeenCalledWith(`Using config: ${configPath}`);
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  });

  it("explicit --config overrides auto-discovery", async () => {
    const tempDir = await mkdtemp(path.join(tmpdir(), "legion-start-explicit-config-"));
    const homeDir = path.join(tempDir, "home");
    const autoConfigPath = path.join(tempDir, "legion.yaml");
    const explicitConfigPath = path.join(tempDir, "explicit.yaml");

    try {
      fs.mkdirSync(homeDir, { recursive: true });
      await writeFile(
        autoConfigPath,
        "project: auto/team\nworkspace: /tmp/auto-workspace\nport: 15433\n"
      );
      await writeFile(
        explicitConfigPath,
        "project: explicit/team\nworkspace: /tmp/explicit-workspace\nport: 15434\n"
      );
      process.chdir(tempDir);
      process.env.HOME = homeDir;
      delete process.env.XDG_CONFIG_HOME;

      try {
        await cmdStart(
          undefined,
          { config: explicitConfigPath, foreground: true },
          {
            startDaemon: async (config) => {
              startDaemonCalls.push(config as unknown as Record<string, unknown>);
              throw new Error(START_DAEMON_ABORT);
            },
            resolveLegionId: async (team, opts) => {
              resolveLegionIdCalls.push({ team, backend: readBackendArg(opts) });
              return resolveLegionIdResult ?? team;
            },
          }
        );
        throw new Error("Expected cmdStart to reject");
      } catch (error) {
        expect(error).toBeInstanceOf(Error);
        expect((error as Error).message).toContain(START_DAEMON_ABORT);
      }

      expect(resolveLegionIdCalls).toHaveLength(0);
      expect(startDaemonCalls).toHaveLength(1);
      expect(startDaemonCalls[0]).toEqual(
        expect.objectContaining({
          legionId: "explicit/team",
          legionDir: "/tmp/explicit-workspace",
          daemonPort: 15434,
          daemonPortExplicit: true,
        })
      );
      expect(console.log).not.toHaveBeenCalledWith(`Using config: ${autoConfigPath}`);
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  });

  it("starts from --config without positional team when config provides project", async () => {
    const tempDir = await mkdtemp(path.join(tmpdir(), "legion-start-config-"));
    const configPath = path.join(tempDir, "legion.yml");

    try {
      await writeFile(
        configPath,
        ["project: acme/12", "workspace: /tmp/config-workspace", "port: 15432"].join("\n")
      );

      try {
        await cmdStart(
          undefined,
          { config: configPath, foreground: true },
          {
            startDaemon: async (config) => {
              startDaemonCalls.push(config as unknown as Record<string, unknown>);
              throw new Error(START_DAEMON_ABORT);
            },
            resolveLegionId: async (team, opts) => {
              resolveLegionIdCalls.push({ team, backend: readBackendArg(opts) });
              return resolveLegionIdResult ?? team;
            },
          }
        );
        throw new Error("Expected cmdStart to reject");
      } catch (error) {
        expect(error).toBeInstanceOf(Error);
        expect((error as Error).message).toContain(START_DAEMON_ABORT);
      }

      expect(resolveLegionIdCalls).toHaveLength(0);
      expect(startDaemonCalls).toHaveLength(1);
      expect(startDaemonCalls[0]).toEqual(
        expect.objectContaining({
          legionId: "acme/12",
          legionDir: "/tmp/config-workspace",
          daemonPort: 15432,
          daemonPortExplicit: true,
        })
      );
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  });

  it("prefers positional team over config project", async () => {
    const tempDir = await mkdtemp(path.join(tmpdir(), "legion-start-config-"));
    const configPath = path.join(tempDir, "legion.yml");
    resolveLegionIdResult = "resolved/team";

    try {
      await writeFile(configPath, "project: from-config/99\nworkspace: /tmp/config-workspace\n");

      try {
        await cmdStart(
          "cli-team",
          { config: configPath, foreground: true },
          {
            startDaemon: async (config) => {
              startDaemonCalls.push(config as unknown as Record<string, unknown>);
              throw new Error(START_DAEMON_ABORT);
            },
            resolveLegionId: async (team, opts) => {
              resolveLegionIdCalls.push({ team, backend: readBackendArg(opts) });
              return resolveLegionIdResult ?? team;
            },
          }
        );
        throw new Error("Expected cmdStart to reject");
      } catch (error) {
        expect(error).toBeInstanceOf(Error);
        expect((error as Error).message).toContain(START_DAEMON_ABORT);
      }

      expect(resolveLegionIdCalls).toEqual([{ team: "cli-team", backend: undefined }]);
      expect(startDaemonCalls).toHaveLength(1);
      expect(startDaemonCalls[0]).toEqual(
        expect.objectContaining({
          legionId: "resolved/team",
          legionDir: "/tmp/config-workspace",
        })
      );
    } finally {
      await rm(tempDir, { recursive: true, force: true });
    }
  });

  it("fails when neither positional team nor config project is provided", async () => {
    const tempDir = await mkdtemp(path.join(tmpdir(), "legion-start-config-"));
    const configPath = path.join(tempDir, "legion.yml");
    const originalLegionId = process.env.LEGION_ID;

    try {
      delete process.env.LEGION_ID;
      await writeFile(configPath, "backend: github\nworkspace: /tmp/config-workspace\n");

      try {
        await cmdStart(
          undefined,
          { config: configPath, foreground: true },
          {
            startDaemon: async (config) => {
              startDaemonCalls.push(config as unknown as Record<string, unknown>);
              throw new Error(START_DAEMON_ABORT);
            },
            resolveLegionId: async (team, opts) => {
              resolveLegionIdCalls.push({ team, backend: readBackendArg(opts) });
              return resolveLegionIdResult ?? team;
            },
          }
        );
        throw new Error("Expected cmdStart to reject");
      } catch (error) {
        expect(error).toBeInstanceOf(Error);
        expect((error as Error).message).toContain(
          "Missing project: provide positional team arg or 'project' in config file"
        );
      }
      expect(startDaemonCalls).toHaveLength(0);
    } finally {
      if (originalLegionId === undefined) {
        delete process.env.LEGION_ID;
      } else {
        process.env.LEGION_ID = originalLegionId;
      }
      await rm(tempDir, { recursive: true, force: true });
    }
  });
});

describe("cmdStart daemonization", () => {
  const originalLog = console.log;
  const originalError = console.error;
  const originalCwd = process.cwd();
  const originalHome = process.env.HOME;
  const originalXdgStateHome = process.env.XDG_STATE_HOME;
  const originalXdgConfigHome = process.env.XDG_CONFIG_HOME;
  let tempDir: string;

  beforeEach(async () => {
    spawnCalls.length = 0;
    spawnPid = 99999;
    tempDir = await mkdtemp(path.join(tmpdir(), "legion-daemonize-"));
    console.log = mock(() => {}) as typeof console.log;
    console.error = mock(() => {}) as typeof console.error;
  });

  afterEach(async () => {
    console.log = originalLog;
    console.error = originalError;
    process.chdir(originalCwd);
    if (originalHome === undefined) {
      delete process.env.HOME;
    } else {
      process.env.HOME = originalHome;
    }
    if (originalXdgStateHome === undefined) {
      delete process.env.XDG_STATE_HOME;
    } else {
      process.env.XDG_STATE_HOME = originalXdgStateHome;
    }
    if (originalXdgConfigHome === undefined) {
      delete process.env.XDG_CONFIG_HOME;
    } else {
      process.env.XDG_CONFIG_HOME = originalXdgConfigHome;
    }
    await rm(tempDir, { recursive: true, force: true });
  });

  it("spawns detached child process when foreground is not set", async () => {
    const homeDir = path.join(tempDir, "home");
    const xdgConfigHome = path.join(tempDir, "xdg-config");
    const xdgStateHome = path.join(tempDir, "xdg-state");
    const configPath = path.join(xdgConfigHome, "legion", "legion.yaml");

    fs.mkdirSync(homeDir, { recursive: true });
    fs.mkdirSync(path.dirname(configPath), { recursive: true });
    await writeFile(configPath, "project: test/team\nworkspace: /tmp/test-workspace\n");

    process.chdir(tempDir);
    process.env.HOME = homeDir;
    process.env.XDG_CONFIG_HOME = xdgConfigHome;
    process.env.XDG_STATE_HOME = xdgStateHome;

    // Write a fake registry entry so waitForDaemonStart finds it
    const legionsFile = path.join(xdgStateHome, "legion", "legions.json");
    fs.mkdirSync(path.dirname(legionsFile), { recursive: true });
    await writeFile(
      legionsFile,
      JSON.stringify({
        "test/team": {
          port: 13370,
          servePort: 13381,
          pid: spawnPid,
          startedAt: new Date().toISOString(),
        },
      })
    );

    await cmdStart(
      undefined,
      {},
      {
        startDaemon: async () => {
          throw new Error("startDaemon should not be called in daemon mode");
        },
        resolveLegionId: async (team) => team,
      }
    );

    // Should have spawned a child process
    expect(spawnCalls).toHaveLength(1);
    const call = spawnCalls[0];
    expect(call.args).toContain("start");
    expect(call.args).toContain("--foreground");
    const opts = call.opts as { detached?: boolean; stdio?: unknown[] };
    expect(opts.detached).toBe(true);

    // Should NOT have called startDaemon
    // (if it did, the test would have thrown)
  });

  it("calls startDaemon directly when foreground is true", async () => {
    const homeDir = path.join(tempDir, "home");
    const xdgConfigHome = path.join(tempDir, "xdg-config");
    const xdgStateHome = path.join(tempDir, "xdg-state");
    const configPath = path.join(xdgConfigHome, "legion", "legion.yaml");

    fs.mkdirSync(homeDir, { recursive: true });
    fs.mkdirSync(path.dirname(configPath), { recursive: true });
    await writeFile(configPath, "project: test/team\nworkspace: /tmp/test-workspace\n");

    process.chdir(tempDir);
    process.env.HOME = homeDir;
    process.env.XDG_CONFIG_HOME = xdgConfigHome;
    process.env.XDG_STATE_HOME = xdgStateHome;

    const startDaemonCalls: unknown[] = [];
    const START_DAEMON_ABORT = "__foreground-abort__";

    try {
      await cmdStart(
        undefined,
        { foreground: true },
        {
          startDaemon: async (config) => {
            startDaemonCalls.push(config);
            throw new Error(START_DAEMON_ABORT);
          },
          resolveLegionId: async (team) => team,
        }
      );
      throw new Error("Expected cmdStart to reject");
    } catch (error) {
      expect((error as Error).message).toContain(START_DAEMON_ABORT);
    }

    // Should have called startDaemon, not spawned a child
    expect(startDaemonCalls).toHaveLength(1);
    expect(spawnCalls).toHaveLength(0);
  });

  it("creates log directory and passes log file to child process", async () => {
    const homeDir = path.join(tempDir, "home");
    const xdgConfigHome = path.join(tempDir, "xdg-config");
    const xdgStateHome = path.join(tempDir, "xdg-state");
    const configPath = path.join(xdgConfigHome, "legion", "legion.yaml");

    fs.mkdirSync(homeDir, { recursive: true });
    fs.mkdirSync(path.dirname(configPath), { recursive: true });
    await writeFile(configPath, "project: test/team\nworkspace: /tmp/test-workspace\n");

    process.chdir(tempDir);
    process.env.HOME = homeDir;
    process.env.XDG_CONFIG_HOME = xdgConfigHome;
    process.env.XDG_STATE_HOME = xdgStateHome;

    // Write a fake registry entry
    const legionsFile = path.join(xdgStateHome, "legion", "legions.json");
    fs.mkdirSync(path.dirname(legionsFile), { recursive: true });
    await writeFile(
      legionsFile,
      JSON.stringify({
        "test/team": {
          port: 13370,
          servePort: 13381,
          pid: spawnPid,
          startedAt: new Date().toISOString(),
        },
      })
    );

    await cmdStart(
      undefined,
      {},
      {
        startDaemon: async () => {
          throw new Error("should not be called");
        },
        resolveLegionId: async (team) => team,
      }
    );

    // Log directory should have been created
    const logDir = path.join(xdgStateHome, "legion", "legions", "test/team", "logs");
    expect(fs.existsSync(logDir)).toBe(true);
  });

  it("passes all CLI flags through to child process", async () => {
    const homeDir = path.join(tempDir, "home");
    const xdgConfigHome = path.join(tempDir, "xdg-config");
    const xdgStateHome = path.join(tempDir, "xdg-state");
    const configPath = path.join(xdgConfigHome, "legion", "legion.yaml");

    fs.mkdirSync(homeDir, { recursive: true });
    fs.mkdirSync(path.dirname(configPath), { recursive: true });
    await writeFile(configPath, "project: test/team\nworkspace: /tmp/test-workspace\n");

    process.chdir(tempDir);
    process.env.HOME = homeDir;
    process.env.XDG_CONFIG_HOME = xdgConfigHome;
    process.env.XDG_STATE_HOME = xdgStateHome;

    // Write a fake registry entry — legionId resolves to "my-team" since resolveLegionId
    // returns the team as-is, and the CLI override takes precedence over config project
    const legionsFile = path.join(xdgStateHome, "legion", "legions.json");
    fs.mkdirSync(path.dirname(legionsFile), { recursive: true });
    await writeFile(
      legionsFile,
      JSON.stringify({
        "my-team": {
          port: 13370,
          servePort: 13381,
          pid: spawnPid,
          startedAt: new Date().toISOString(),
        },
      })
    );

    await cmdStart(
      "my-team",
      {
        workspace: "/tmp/ws",
        prompt: "custom prompt",
        backend: "github",
        runtime: "opencode",
        config: configPath,
      },
      {
        startDaemon: async () => {
          throw new Error("should not be called");
        },
        resolveLegionId: async (team) => team,
      }
    );

    expect(spawnCalls).toHaveLength(1);
    const args = spawnCalls[0].args;
    expect(args).toContain("start");
    expect(args).toContain("my-team");
    expect(args).toContain("--workspace");
    expect(args).toContain("/tmp/ws");
    expect(args).toContain("--prompt");
    expect(args).toContain("custom prompt");
    expect(args).toContain("--backend");
    expect(args).toContain("github");
    expect(args).toContain("--runtime");
    expect(args).toContain("opencode");
    expect(args).toContain("--config");
    expect(args).toContain(configPath);
    expect(args).toContain("--foreground");
  });
});

describe("enlistCommand", () => {
  it("has correct meta", async () => {
    const meta = await Promise.resolve(enlistCommand.meta);
    expect(meta?.name).toBe("enlist");
  });

  it("requires team and session as positional args", async () => {
    const args = await resolveArgs(enlistCommand);
    expect(args.team).toEqual(expect.objectContaining({ type: "positional", required: true }));
    expect(args.session).toEqual(expect.objectContaining({ type: "positional", required: true }));
  });

  it("requires --mode and --issue flags", async () => {
    const args = await resolveArgs(enlistCommand);
    expect(args.mode).toEqual(expect.objectContaining({ type: "string", required: true }));
    expect(args.issue).toEqual(expect.objectContaining({ type: "string", required: true }));
  });

  it("has optional --workspace flag", async () => {
    const args = await resolveArgs(enlistCommand);
    expect(args.workspace).toEqual(expect.objectContaining({ type: "string" }));
    expect((args.workspace as StringArg).required).toBeFalsy();
  });
});

describe("cmdEnlist behavior", () => {
  const originalFetch = globalThis.fetch;
  let capturedCalls: Array<{ url: string; body: Record<string, unknown> }>;

  beforeEach(() => {
    capturedCalls = [];
    const mockFn = async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/health")) {
        return new Response(JSON.stringify({ status: "ok" }), { status: 200 });
      }
      if (url.includes("/workers") && init?.method === "POST") {
        const body = JSON.parse(init.body as string) as Record<string, unknown>;
        capturedCalls.push({ url, body });
        return new Response(
          JSON.stringify({
            id: `${body.issueId}-${body.mode}`,
            port: 13381,
            sessionId: body.sessionId,
            promptDelivered: true,
          }),
          { status: 200 }
        );
      }
      return originalFetch(input, init);
    };
    globalThis.fetch = Object.assign(mockFn, {
      preconnect: originalFetch.preconnect,
    });
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it("sends sessionId, force=true, and prompt in POST body", async () => {
    const testSession = "ses_31617365bffeUEa4wPBVIL2LBI";
    await cmdEnlist("sjawhar/5", testSession, {
      mode: "implement",
      issue: "eng-42",
      workspace: "/tmp/test-workspace",
      daemonPort: 13370,
    });

    expect(capturedCalls).toHaveLength(1);
    expect(capturedCalls[0]?.body).toEqual(
      expect.objectContaining({
        issueId: "eng-42",
        mode: "implement",
        sessionId: testSession,
        force: true,
        prompt: "/legion-worker implement mode for eng-42",
        workspace: "/tmp/test-workspace",
      })
    );
  });

  it("throws CliError for session_already_enlisted 409", () => {
    const mock409 = async (input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/health")) {
        return new Response(JSON.stringify({ status: "ok" }), { status: 200 });
      }
      if (url.includes("/workers") && init?.method === "POST") {
        return new Response(
          JSON.stringify({ error: "session_already_enlisted", id: "eng-42-implement" }),
          { status: 409 }
        );
      }
      return originalFetch(input, init);
    };
    globalThis.fetch = Object.assign(mock409, {
      preconnect: originalFetch.preconnect,
    });

    return expect(
      cmdEnlist("sjawhar/5", "ses_31617365bffeUEa4wPBVIL2LBI", {
        mode: "implement",
        issue: "eng-42",
        workspace: "/tmp/work",
        daemonPort: 13370,
      })
    ).rejects.toThrow("already tracked by worker");
  });

  it("throws CliError for invalid session ID format", () => {
    return expect(
      cmdEnlist("sjawhar/5", "not-a-session-id", {
        mode: "implement",
        issue: "eng-42",
        workspace: "/tmp/work",
        daemonPort: 13370,
      })
    ).rejects.toThrow("Invalid session ID format");
  });
});

describe("citty command definitions", () => {
  test("start command args are defined", async () => {
    const args = await resolveArgs(startCommand);
    const team = args.team as PositionalArg;
    const config = args.config as StringArg;
    const workspace = args.workspace as StringArg;
    const stateDir = args["state-dir"] as StringArg;
    const foreground = args.foreground as { type: string; alias: string; default: boolean };
    expect(team.type).toBe("positional");
    expect(team.required).toBe(false);
    expect(config).toEqual(expect.objectContaining({ type: "string", alias: "c" }));
    expect(workspace.type).toBe("string");
    expect(workspace.alias).toBe("w");
    expect(workspace.default).toBeUndefined();
    expect(stateDir.type).toBe("string");
    expect(foreground).toEqual(
      expect.objectContaining({ type: "boolean", alias: "f", default: false })
    );
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
    const args = await resolveArgs(legionsCommand);
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
    const repo = args.repo as StringArg;
    const version = args.version as NumberArg;
    expect(issue.type).toBe("positional");
    expect(issue.required).toBe(true);
    expect(mode.type).toBe("positional");
    expect(mode.required).toBe(true);
    expect(prompt.type).toBe("string");
    expect(workspace.type).toBe("string");
    expect(workspace.alias).toBe("w");
    expect(repo.type).toBe("string");
    expect(repo.alias).toBe("r");
    expect(version.type).toBe("string");
    expect(version.alias).toBe("v");
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
  const originalLog = console.log;
  const originalError = console.error;
  const originalWarn = console.warn;

  beforeEach(() => {
    console.log = mock(() => {}) as typeof console.log;
    console.error = mock(() => {}) as typeof console.error;
    console.warn = mock(() => {}) as typeof console.warn;
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    console.log = originalLog;
    console.error = originalError;
    console.warn = originalWarn;
  });

  it("dispatches worker via daemon API with repo and sends prompt via POST body", async () => {
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
          new Response(
            JSON.stringify({
              id: "leg-42-implement",
              port: 18000,
              sessionId: "s-1",
              promptDelivered: true,
            }),
            {
              status: 200,
              headers: { "content-type": "application/json" },
            }
          )
        );
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });
    await cmdDispatch("LEG-42", "implement", {
      daemonPort: 13371,
      repo: "sjawhar/legion",
    });

    expect(fetchMock.mock.calls.length).toBe(2);
    const [postUrl, postInit] = fetchMock.mock.calls[1] as FetchCall;
    const postInitResolved = postInit as RequestInit;
    expect(postUrl.toString()).toBe("http://127.0.0.1:13371/workers");
    expect(postInitResolved.method).toBe("POST");
    const body = JSON.parse(postInitResolved.body as string) as {
      issueId: string;
      mode: string;
      repo: string;
      prompt: string;
    };
    expect(body).toEqual({
      issueId: "LEG-42",
      mode: "implement",
      repo: "sjawhar/legion",
      prompt: "/legion-worker implement mode for LEG-42",
    });
  });

  it("sends env in POST body when provided", async () => {
    const fetchMock = installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.resolve(
        new Response(JSON.stringify({ id: "leg-42-implement", port: 18000, sessionId: "s-env" }), {
          status: 200,
          headers: { "content-type": "application/json" },
        })
      );
    });
    await cmdDispatch("LEG-42", "implement", {
      daemonPort: 13371,
      repo: "sjawhar/legion",
      env: { GH_TOKEN: "ghs_test", GIT_AUTHOR_NAME: "bot[bot]" },
    });

    const [, postInit] = fetchMock.mock.calls[1] as FetchCall;
    const body = JSON.parse((postInit as RequestInit).body as string) as Record<string, unknown>;
    expect(body.env).toEqual({ GH_TOKEN: "ghs_test", GIT_AUTHOR_NAME: "bot[bot]" });
  });

  it("does not include env in POST body when not provided", async () => {
    const fetchMock = installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.resolve(
        new Response(
          JSON.stringify({ id: "leg-42-implement", port: 18000, sessionId: "s-noenv" }),
          { status: 200, headers: { "content-type": "application/json" } }
        )
      );
    });
    await cmdDispatch("LEG-42", "implement", {
      daemonPort: 13371,
      repo: "sjawhar/legion",
    });

    const [, postInit] = fetchMock.mock.calls[1] as FetchCall;
    const body = JSON.parse((postInit as RequestInit).body as string) as Record<string, unknown>;
    expect(body).not.toHaveProperty("env");
  });

  it("uses workspace for backward compatibility when provided", async () => {
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
    await cmdDispatch("LEG-42", "implement", {
      daemonPort: 13372,
      workspace: "/tmp/legacy-workspace",
    });

    const [, postInit] = fetchMock.mock.calls[1] as FetchCall;
    const body = JSON.parse((postInit as RequestInit).body as string) as {
      issueId: string;
      mode: string;
      workspace: string;
    };
    expect(body.workspace).toBe("/tmp/legacy-workspace");
  });

  it("fails when neither repo nor workspace is provided", () => {
    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(cmdDispatch("LEG-42", "implement", { daemonPort: 13372 })).rejects.toThrow(
      "Either --repo or --workspace is required"
    );
  });

  it("threads version into worker creation request", async () => {
    const tempDir = createTempDir();
    const legionDir = path.join(tempDir, "legion");
    fs.mkdirSync(legionDir);

    const fetchMock = installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: "leg-42-implement",
              port: 18000,
              sessionId: "s-v2",
              promptDelivered: true,
            }),
            {
              status: 200,
              headers: { "content-type": "application/json" },
            }
          )
        );
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    await cmdDispatch("LEG-42", "implement", { legionDir, daemonPort: 13387, version: 2 });

    const [, postInit] = fetchMock.mock.calls[1] as FetchCall;
    const body = JSON.parse((postInit as RequestInit).body as string) as {
      issueId: string;
      mode: string;
      workspace: string;
      version: number;
      prompt: string;
    };
    expect(body).toEqual({
      issueId: "LEG-42",
      mode: "implement",
      workspace: legionDir,
      version: 2,
      prompt: "/legion-worker implement mode for LEG-42",
    });
  });

  it("rejects invalid version values", () => {
    return expect(
      cmdDispatch("LEG-42", "implement", { daemonPort: 13370, version: -1 })
    ).rejects.toThrow("Invalid version");
  });

  it("fails gracefully when daemon is not running", async () => {
    installFetchMock(() => {
      throw new Error("ECONNREFUSED");
    });

    return expect(
      cmdDispatch("LEG-42", "implement", { daemonPort: 13373, repo: "sjawhar/legion" })
    ).rejects.toThrow(CliError);
  });

  it("reports 409 when worker already exists", async () => {
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
    await cmdDispatch("LEG-42", "implement", { daemonPort: 13374, repo: "sjawhar/legion" });

    expect(fetchMock.mock.calls.length).toBe(2);
  });

  it("reports 429 when crash limit exceeded", async () => {
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
      cmdDispatch("LEG-42", "implement", { daemonPort: 13375, repo: "sjawhar/legion" })
    ).rejects.toThrow(CliError);
  });

  it("warns but succeeds when server reports prompt delivery failure", async () => {
    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response(JSON.stringify({ status: "ok" }), { status: 200 }));
      }
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: "leg-42-implement",
              port: 18000,
              sessionId: "s-6",
              promptDelivered: false,
            }),
            {
              status: 200,
              headers: { "content-type": "application/json" },
            }
          )
        );
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    await cmdDispatch("LEG-42", "implement", {
      daemonPort: 13377,
      repo: "sjawhar/legion",
    });

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
    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(new Response("unhealthy", { status: 500 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });

    return expect(
      cmdDispatch("LEG-42", "implement", { daemonPort: 13382, repo: "sjawhar/legion" })
    ).rejects.toThrow("Daemon is not healthy");
  });

  it("fails when daemon worker creation cannot connect", () => {
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
      cmdDispatch("LEG-42", "implement", { daemonPort: 13384, repo: "sjawhar/legion" })
    ).rejects.toThrow("Could not connect to daemon");
  });

  it("fails when daemon returns non-JSON response", () => {
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
      cmdDispatch("LEG-42", "implement", { daemonPort: 13385, repo: "sjawhar/legion" })
    ).rejects.toThrow("Daemon returned non-JSON response (status 200)");
  });

  it("fails when daemon returns non-OK status", () => {
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
      cmdDispatch("LEG-42", "implement", { daemonPort: 13386, repo: "sjawhar/legion" })
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
    spawnCalls.length = 0;
    spawnExitCode = 0;
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

  it("logs tmux attach command for claude-code runtime", async () => {
    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              status: "ok",
              runtime: "claude-code",
              tmuxSession: "legion-abc123",
            }),
            { status: 200, headers: { "content-type": "application/json" } }
          )
        );
      }
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(JSON.stringify([{ id: "leg-42-implement", port: 2000 }]), {
            status: 200,
            headers: { "content-type": "application/json" },
          })
        );
      }
      return Promise.reject(new Error(`Unexpected: ${url}`));
    });

    try {
      await Promise.resolve(
        runCommand(attachCommand, {
          team: "12345678-1234-1234-1234-123456789abc",
          issue: "leg-42-implement",
        })
      );
    } catch {}

    const logCalls = (console.log as ReturnType<typeof mock>).mock.calls.flat();
    expect(logCalls.join("\n")).toContain("tmux attach -t legion-abc123");
    expect(spawnCalls.some((c) => c.cmd === "tmux" && c.args.includes("legion-abc123"))).toBe(true);
  });

  it("logs opencode attach command for opencode runtime", async () => {
    installFetchMock((input: string | URL) => {
      const url = input.toString();
      if (url.endsWith("/health")) {
        return Promise.resolve(
          new Response(JSON.stringify({ status: "ok", runtime: "opencode" }), {
            status: 200,
            headers: { "content-type": "application/json" },
          })
        );
      }
      if (url.endsWith("/workers")) {
        return Promise.resolve(
          new Response(JSON.stringify([{ id: "leg-42-implement", port: 2000 }]), {
            status: 200,
            headers: { "content-type": "application/json" },
          })
        );
      }
      return Promise.reject(new Error(`Unexpected: ${url}`));
    });

    try {
      await Promise.resolve(
        runCommand(attachCommand, {
          team: "12345678-1234-1234-1234-123456789abc",
          issue: "leg-42-implement",
        })
      );
    } catch {}

    const logCalls = (console.log as ReturnType<typeof mock>).mock.calls.flat();
    expect(logCalls.join("\n")).toContain("opencode attach http://localhost:2000");
    expect(spawnCalls.some((c) => c.cmd === "opencode")).toBe(true);
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
      if (url.endsWith("/workers/leg-42-implement/prompt")) {
        return Promise.resolve(new Response("{}", { status: 200 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });
    await cmdPrompt("LEG-42", "Check the issue comments", { daemonPort: 13376 });

    expect(fetchMock.mock.calls.length).toBe(2);
    const [secondUrl, secondInit] = fetchMock.mock.calls[1] as FetchCall;
    expect(secondUrl.toString()).toBe("http://127.0.0.1:13376/workers/leg-42-implement/prompt");
    expect(JSON.parse((secondInit as RequestInit).body as string)).toEqual({
      text: "Check the issue comments",
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
      if (url.endsWith("/workers/leg-42-implement/prompt")) {
        return Promise.resolve(new Response("{}", { status: 200 }));
      }
      return Promise.reject(new Error(`Unexpected fetch: ${url}`));
    });
    await cmdPrompt("LEG-42", "Check the issue comments", {
      daemonPort: 13377,
      mode: "implement",
    });

    const [secondUrl] = fetchMock.mock.calls[1] as FetchCall;
    expect(secondUrl.toString()).toBe("http://127.0.0.1:13377/workers/leg-42-implement/prompt");
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
      if (url.endsWith("/workers/leg-42-implement/prompt")) {
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
      if (url.endsWith("/workers/leg-42-implement/prompt")) {
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
  test("returns default port when env unset", async () => {
    expect(await getDaemonPort()).toBe(13370);
  });

  test("returns env port when valid", async () => {
    const originalPort = process.env.LEGION_DAEMON_PORT;
    process.env.LEGION_DAEMON_PORT = "14400";
    try {
      expect(await getDaemonPort()).toBe(14400);
    } finally {
      if (originalPort === undefined) {
        delete process.env.LEGION_DAEMON_PORT;
      } else {
        process.env.LEGION_DAEMON_PORT = originalPort;
      }
    }
  });

  test("falls back when env port invalid", async () => {
    const originalPort = process.env.LEGION_DAEMON_PORT;
    process.env.LEGION_DAEMON_PORT = "not-a-number";
    try {
      expect(await getDaemonPort()).toBe(13370);
    } finally {
      if (originalPort === undefined) {
        delete process.env.LEGION_DAEMON_PORT;
      } else {
        process.env.LEGION_DAEMON_PORT = originalPort;
      }
    }
  });

  test("reads daemon port from legions registry when projectId provided", async () => {
    const stateHome = fs.mkdtempSync(path.join(os.tmpdir(), "legion-state-home-"));
    const originalStateHome = process.env.XDG_STATE_HOME;
    const originalPort = process.env.LEGION_DAEMON_PORT;

    delete process.env.LEGION_DAEMON_PORT;
    process.env.XDG_STATE_HOME = stateHome;
    const legionsFile = path.join(stateHome, "legion", "legions.json");
    fs.mkdirSync(path.dirname(legionsFile), { recursive: true });
    fs.writeFileSync(
      legionsFile,
      JSON.stringify(
        {
          "sjawhar/42": {
            port: 15555,
            servePort: 15566,
            pid: process.pid,
            startedAt: "2026-01-01T00:00:00.000Z",
          },
        },
        null,
        2
      )
    );

    try {
      expect(await getDaemonPort("sjawhar/42")).toBe(15555);
    } finally {
      fs.rmSync(stateHome, { recursive: true, force: true });
      if (originalStateHome === undefined) {
        delete process.env.XDG_STATE_HOME;
      } else {
        process.env.XDG_STATE_HOME = originalStateHome;
      }
      if (originalPort === undefined) {
        delete process.env.LEGION_DAEMON_PORT;
      } else {
        process.env.LEGION_DAEMON_PORT = originalPort;
      }
    }
  });

  describe("characterization: getDaemonPort", () => {
    test("returns port from registry when project entry exists", async () => {
      const stateHome = fs.mkdtempSync(path.join(os.tmpdir(), "legion-state-home-"));
      const originalStateHome = process.env.XDG_STATE_HOME;
      const originalPort = process.env.LEGION_DAEMON_PORT;

      delete process.env.LEGION_DAEMON_PORT;
      process.env.XDG_STATE_HOME = stateHome;
      const legionsFile = path.join(stateHome, "legion", "legions.json");
      fs.mkdirSync(path.dirname(legionsFile), { recursive: true });
      fs.writeFileSync(
        legionsFile,
        JSON.stringify(
          {
            "acme/77": {
              port: 16666,
              servePort: 17777,
              pid: process.pid,
              startedAt: "2026-01-01T00:00:00.000Z",
            },
          },
          null,
          2
        )
      );

      try {
        expect(await getDaemonPort("acme/77")).toBe(16666);
      } finally {
        fs.rmSync(stateHome, { recursive: true, force: true });
        if (originalStateHome === undefined) {
          delete process.env.XDG_STATE_HOME;
        } else {
          process.env.XDG_STATE_HOME = originalStateHome;
        }
        if (originalPort === undefined) {
          delete process.env.LEGION_DAEMON_PORT;
        } else {
          process.env.LEGION_DAEMON_PORT = originalPort;
        }
      }
    });

    test("returns default port when registry has no project entry", async () => {
      const stateHome = fs.mkdtempSync(path.join(os.tmpdir(), "legion-state-home-"));
      const originalStateHome = process.env.XDG_STATE_HOME;
      const originalPort = process.env.LEGION_DAEMON_PORT;

      delete process.env.LEGION_DAEMON_PORT;
      process.env.XDG_STATE_HOME = stateHome;
      const legionsFile = path.join(stateHome, "legion", "legions.json");
      fs.mkdirSync(path.dirname(legionsFile), { recursive: true });
      fs.writeFileSync(
        legionsFile,
        JSON.stringify(
          {
            "acme/77": {
              port: 16666,
              servePort: 17777,
              pid: process.pid,
              startedAt: "2026-01-01T00:00:00.000Z",
            },
          },
          null,
          2
        )
      );

      try {
        expect(await getDaemonPort("missing/project")).toBe(13370);
      } finally {
        fs.rmSync(stateHome, { recursive: true, force: true });
        if (originalStateHome === undefined) {
          delete process.env.XDG_STATE_HOME;
        } else {
          process.env.XDG_STATE_HOME = originalStateHome;
        }
        if (originalPort === undefined) {
          delete process.env.LEGION_DAEMON_PORT;
        } else {
          process.env.LEGION_DAEMON_PORT = originalPort;
        }
      }
    });
  });
});

describe("loadLegionsCache", () => {
  test("returns null when cache missing", () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cache-"));
    try {
      expect(loadLegionsCache(tempDir)).toBeNull();
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
    }
  });

  test("loads teams cache from disk", () => {
    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "legion-cache-"));
    try {
      const cacheFile = path.join(tempDir, "project-cache.json");
      const payload = {
        LEG: { id: "uuid-123", name: "Legion" },
      };
      fs.writeFileSync(cacheFile, JSON.stringify(payload, null, 2));

      const result = loadLegionsCache(tempDir);
      expect(result).toEqual(payload);
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
    }
  });
});

describe("CLI XDG path migration", () => {
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

  test("cmdStart path derivation uses XDG, not legacy dotdir", () => {
    const stateHome = fs.mkdtempSync(path.join(os.tmpdir(), "legion-xdg-start-"));
    const originalStateHome = process.env.XDG_STATE_HOME;
    process.env.XDG_STATE_HOME = stateHome;

    try {
      const legionId = "12345678-1234-1234-1234-123456789abc";
      // This is the exact same call cmdStart makes after the XDG migration
      const instancePaths = resolveLegionPaths(process.env, os.homedir()).forLegion(legionId);

      // Verify paths are XDG-based, not legacy
      expect(instancePaths.workersFile).toContain(stateHome);
      expect(instancePaths.workersFile).not.toContain(".legion");
      expect(instancePaths.legionStateDir).toContain(stateHome);
      expect(instancePaths.legionStateDir).not.toContain(".legion");
    } finally {
      if (originalStateHome === undefined) {
        delete process.env.XDG_STATE_HOME;
      } else {
        process.env.XDG_STATE_HOME = originalStateHome;
      }
      fs.rmSync(stateHome, { recursive: true, force: true });
    }
  });

  test("cmdStatus reads state from XDG path, not legacy path", async () => {
    const stateHome = fs.mkdtempSync(path.join(os.tmpdir(), "legion-xdg-status-"));
    const originalStateHome = process.env.XDG_STATE_HOME;
    process.env.XDG_STATE_HOME = stateHome;

    try {
      const legionId = "12345678-1234-1234-1234-123456789abc";
      const legionStateDir = path.join(stateHome, "legion", "legions", legionId);
      fs.mkdirSync(legionStateDir, { recursive: true });
      const workersFile = path.join(legionStateDir, "workers.json");
      fs.writeFileSync(workersFile, JSON.stringify([]));

      // Mock fetch to simulate daemon not running
      installFetchMock(() => {
        throw new Error("ECONNREFUSED");
      });

      await runCommand(statusCommand, { team: legionId });

      // Verify console.log mentions the XDG state file path
      const logCalls = (console.log as ReturnType<typeof mock>).mock.calls.flat();
      const stateFileLog = logCalls.find(
        (line: unknown) => typeof line === "string" && line.includes("State file:")
      ) as string | undefined;
      expect(stateFileLog).toBeDefined();
      expect(stateFileLog).toContain(stateHome);
      expect(stateFileLog).not.toContain(".legion");
    } finally {
      if (originalStateHome === undefined) {
        delete process.env.XDG_STATE_HOME;
      } else {
        process.env.XDG_STATE_HOME = originalStateHome;
      }
      fs.rmSync(stateHome, { recursive: true, force: true });
    }
  });
});

describe("parseEnvJson", () => {
  it("rejects invalid JSON", () => {
    expect(() => parseEnvJson("not json")).toThrow("Invalid --env value: must be valid JSON");
  });

  it("rejects non-string values", () => {
    expect(() => parseEnvJson('{"KEY": 123}')).toThrow(
      'Invalid --env value: key "KEY" must have a string value'
    );
  });

  it("rejects array value", () => {
    expect(() => parseEnvJson('["a", "b"]')).toThrow("Invalid --env value: must be a JSON object");
  });

  it("parses valid JSON object", () => {
    const result = parseEnvJson('{"KEY": "VALUE"}');
    expect(result).toEqual({ KEY: "VALUE" });
  });
});

// =============================================================================
// Rollback Command Tests
// =============================================================================

describe("parseIssueIdParts (used by rollback)", () => {
  it("parses standard issue ID into owner/repo/number", () => {
    const parts = parseIssueIdParts("sjawhar-legion-526");
    expect(parts).toEqual({ owner: "sjawhar", repo: "legion", number: "526" });
  });

  it("handles multi-segment repo names", () => {
    const parts = parseIssueIdParts("acme-my-project-42");
    expect(parts).toEqual({ owner: "acme", repo: "my-project", number: "42" });
  });

  it("throws for IDs without enough segments", () => {
    expect(() => parseIssueIdParts("singleword-42")).toThrow("Cannot parse issueId");
  });

  it("throws for empty string", () => {
    expect(() => parseIssueIdParts("")).toThrow("Cannot parse issueId");
  });
});

describe("rollbackCommand", () => {
  it("has correct command metadata", async () => {
    const meta = (await Promise.resolve(rollbackCommand.meta)) as
      | { name?: string; description?: string }
      | undefined;
    expect(meta?.name).toBe("rollback");
    expect(meta?.description).toBe("Revert a merged PR and reopen the issue");
  });

  it("has required positional issue arg", async () => {
    const args = await resolveArgs(rollbackCommand);
    const issueArg = args.issue as PositionalArg;
    expect(issueArg.type).toBe("positional");
    expect(issueArg.required).toBe(true);
  });

  it("has optional repo flag", async () => {
    const args = await resolveArgs(rollbackCommand);
    const repoArg = args.repo as StringArg;
    expect(repoArg.type).toBe("string");
    expect(repoArg.alias).toBe("r");
  });

  it("has dry-run flag defaulting to false", async () => {
    const args = await resolveArgs(rollbackCommand);
    const dryRunArg = args["dry-run"] as BooleanArg;
    expect(dryRunArg.type).toBe("boolean");
    expect(dryRunArg.default).toBe(false);
  });
});

describe("cmdRollback", () => {
  beforeEach(() => {
    ghCommandCalls.length = 0;
    ghCommandResponses = [];
  });

  it("rejects invalid issue identifiers", async () => {
    await expect(cmdRollback("invalid identifier!", {})).rejects.toThrow(
      "Invalid issue identifier"
    );
  });

  it("rejects issue IDs that cannot derive a repo without --repo", async () => {
    await expect(cmdRollback("42", {})).rejects.toThrow("Could not derive repo from issue ID");
  });

  it("rejects issue IDs that cannot derive a number with --repo", async () => {
    await expect(cmdRollback("no-number", { repo: "owner/repo" })).rejects.toThrow(
      "Could not derive issue number"
    );
  });

  it("rejects two-segment IDs without --repo", async () => {
    await expect(cmdRollback("singleword-42", {})).rejects.toThrow(
      "Could not derive repo from issue ID"
    );
  });

  it("happy path: finds PR, creates revert, opens revert PR, reopens issue", async () => {
    const mergeOid = "abc123def456";
    const mergedPR = JSON.stringify([
      {
        number: 100,
        title: "feat: add widget",
        mergeCommit: { oid: mergeOid },
        headRefName: "sjawhar-legion-42",
      },
    ]);
    ghCommandResponses = [
      // findMergedPR: pr list --search
      mergedPR,
      // createRevertCommit: get main SHA (must match mergeOid for guard)
      `${mergeOid}\n`,
      // createRevertCommit: create branch ref
      "{}",
      // createRevertCommit: get merge commit parents
      "parent-sha-111\n",
      // createRevertCommit: get parent tree SHA
      "tree-sha-222\n",
      // createRevertCommit: create revert commit
      JSON.stringify({ sha: "revert-sha-333" }),
      // createRevertCommit: update branch ref
      "{}",
      // cmdRollback: create revert PR
      "https://github.com/sjawhar/legion/pull/101\n",
      // cmdRollback: add rollback label
      "",
      // cmdRollback: reopen issue
      "",
    ];

    await cmdRollback("sjawhar-legion-42", {});

    // Verify findMergedPR was called with correct search
    expect(ghCommandCalls[0]).toEqual([
      "pr",
      "list",
      "--search",
      "is:merged 42 in:title",
      "--json",
      "number,title,mergeCommit,headRefName",
      "--limit",
      "5",
      "-R",
      "sjawhar/legion",
    ]);

    // Verify revert PR creation
    const prCreateCall = ghCommandCalls.find((c) => c[0] === "pr" && c[1] === "create");
    expect(prCreateCall).toBeDefined();
    expect(prCreateCall).toContain("--head");
    expect(prCreateCall).toContain("main");

    // Verify issue label and reopen
    const labelCall = ghCommandCalls.find(
      (c) => c[0] === "issue" && c[1] === "edit" && c.includes("--add-label")
    );
    expect(labelCall).toBeDefined();
    expect(labelCall).toContain("rollback");

    const reopenCall = ghCommandCalls.find((c) => c[0] === "issue" && c[1] === "reopen");
    expect(reopenCall).toBeDefined();
    expect(reopenCall).toContain("42");
  });

  it("--dry-run finds PR but does not execute mutating commands", async () => {
    const mergedPR = JSON.stringify([
      {
        number: 100,
        title: "feat: add widget",
        mergeCommit: { oid: "abc123def456" },
        headRefName: "sjawhar-legion-42",
      },
    ]);
    ghCommandResponses = [
      // findMergedPR: pr list --search
      mergedPR,
    ];

    await cmdRollback("sjawhar-legion-42", { dryRun: true });

    // Only the findMergedPR call should have been made — no revert, no PR create, no reopen
    expect(ghCommandCalls).toHaveLength(1);
    expect(ghCommandCalls[0][0]).toBe("pr");
    expect(ghCommandCalls[0][1]).toBe("list");
  });

  it("warns but continues when label addition fails", async () => {
    const mergeOid = "abc123def456";
    const mergedPR = JSON.stringify([
      {
        number: 100,
        title: "feat: add widget",
        mergeCommit: { oid: mergeOid },
        headRefName: "sjawhar-legion-42",
      },
    ]);
    ghCommandResponses = [
      mergedPR,
      `${mergeOid}\n`,
      "{}",
      "parent-sha-111\n",
      "tree-sha-222\n",
      JSON.stringify({ sha: "revert-sha-333" }),
      "{}",
      "https://github.com/sjawhar/legion/pull/101\n",
      // label addition fails
      new Error("label not found"),
      // reopen still succeeds
      "",
    ];

    // Should not throw despite label failure
    await cmdRollback("sjawhar-legion-42", {});

    // Reopen should still have been called
    const reopenCall = ghCommandCalls.find((c) => c[0] === "issue" && c[1] === "reopen");
    expect(reopenCall).toBeDefined();
  });

  it("warns but continues when issue reopen fails", async () => {
    const mergeOid = "abc123def456";
    const mergedPR = JSON.stringify([
      {
        number: 100,
        title: "feat: add widget",
        mergeCommit: { oid: mergeOid },
        headRefName: "sjawhar-legion-42",
      },
    ]);
    ghCommandResponses = [
      mergedPR,
      `${mergeOid}\n`,
      "{}",
      "parent-sha-111\n",
      "tree-sha-222\n",
      JSON.stringify({ sha: "revert-sha-333" }),
      "{}",
      "https://github.com/sjawhar/legion/pull/101\n",
      // label succeeds
      "",
      // reopen fails
      new Error("issue already open"),
    ];

    // Should not throw despite reopen failure
    await cmdRollback("sjawhar-legion-42", {});
  });

  it("throws when main has advanced past merge commit", async () => {
    const mergedPR = JSON.stringify([
      {
        number: 100,
        title: "feat: add widget",
        mergeCommit: { oid: "merge-sha-original" },
        headRefName: "sjawhar-legion-42",
      },
    ]);
    ghCommandResponses = [
      // findMergedPR: pr list --search
      mergedPR,
      // createRevertCommit: get main SHA (different from merge commit — main advanced)
      "main-sha-advanced\n",
    ];

    await expect(cmdRollback("sjawhar-legion-42", {})).rejects.toThrow(
      "Main has advanced past merge commit"
    );

    // Should only have made 2 calls: findMergedPR + get main SHA
    expect(ghCommandCalls).toHaveLength(2);
  });
});

describe("findMergedPR", () => {
  beforeEach(() => {
    ghCommandCalls.length = 0;
    ghCommandResponses = [];
  });

  it("finds PR by title search", async () => {
    const prs = [
      {
        number: 100,
        title: "feat(cli): issue #42 fix",
        mergeCommit: { oid: "abc123" },
        headRefName: "sjawhar-legion-42",
      },
    ];
    ghCommandResponses = [JSON.stringify(prs)];

    const result = await findMergedPR("sjawhar-legion-42", 42, "sjawhar/legion");
    expect(result.number).toBe(100);
    expect(result.mergeCommit?.oid).toBe("abc123");
    expect(ghCommandCalls).toHaveLength(1);
  });

  it("falls back to branch name search when title search returns empty", async () => {
    ghCommandResponses = [
      // Title search returns empty
      "[]",
      // Branch search finds it
      JSON.stringify([
        {
          number: 200,
          title: "some PR",
          mergeCommit: { oid: "def456" },
          headRefName: "sjawhar-legion-42",
        },
      ]),
    ];

    const result = await findMergedPR("sjawhar-legion-42", 42, "sjawhar/legion");
    expect(result.number).toBe(200);
    expect(ghCommandCalls).toHaveLength(2);
    // Second call should search by branch name
    expect(ghCommandCalls[1]).toContain("--search");
    expect(ghCommandCalls[1]).toContain("is:merged head:sjawhar-legion-42");
  });

  it("throws when no merged PR found by either search", async () => {
    ghCommandResponses = [
      // Title search empty
      "[]",
      // Branch search empty
      "[]",
    ];

    await expect(findMergedPR("sjawhar-legion-42", 42, "sjawhar/legion")).rejects.toThrow(
      "No merged PR found"
    );
  });

  it("returns first match when multiple PRs found", async () => {
    const prs = [
      {
        number: 100,
        title: "first PR",
        mergeCommit: { oid: "first" },
        headRefName: "branch-1",
      },
      {
        number: 200,
        title: "second PR",
        mergeCommit: { oid: "second" },
        headRefName: "branch-2",
      },
    ];
    ghCommandResponses = [JSON.stringify(prs)];

    const result = await findMergedPR("sjawhar-legion-42", 42, "sjawhar/legion");
    expect(result.number).toBe(100);
  });

  it("throws on unparseable JSON response", async () => {
    ghCommandResponses = ["not valid json"];

    await expect(findMergedPR("sjawhar-legion-42", 42, "sjawhar/legion")).rejects.toThrow(
      "Failed to parse PR list"
    );
  });
});

describe("createRevertCommit", () => {
  beforeEach(() => {
    ghCommandCalls.length = 0;
    ghCommandResponses = [];
  });

  it("throws when PR has no merge commit SHA", async () => {
    await expect(
      createRevertCommit(
        "sjawhar/legion",
        { number: 100, title: "test", mergeCommit: null, headRefName: "branch" },
        42,
        "sjawhar-legion-42"
      )
    ).rejects.toThrow("has no merge commit SHA");
  });

  it("creates revert commit with correct API call sequence", async () => {
    const mergeOid = "merge-sha-abc";
    ghCommandResponses = [
      // Get main SHA (must match mergeOid for guard)
      `${mergeOid}\n`,
      // Create branch ref
      "{}",
      // Get merge commit parents
      "parent-sha-111\n",
      // Get parent tree SHA
      "tree-sha-222\n",
      // Create revert commit
      JSON.stringify({ sha: "revert-sha-333" }),
      // Update branch ref
      "{}",
    ];

    const result = await createRevertCommit(
      "sjawhar/legion",
      {
        number: 100,
        title: "feat: add widget",
        mergeCommit: { oid: mergeOid },
        headRefName: "sjawhar-legion-42",
      },
      42,
      "sjawhar-legion-42"
    );

    expect(result.revertCommitSha).toBe("revert-sha-333");
    expect(result.revertBranch).toMatch(/^revert-sjawhar-legion-42-\d+$/);

    // Verify API call sequence
    // 1. Get main SHA
    expect(ghCommandCalls[0]).toContain("repos/sjawhar/legion/git/ref/heads/main");
    // 2. Create branch
    expect(ghCommandCalls[1]).toContain("-X");
    expect(ghCommandCalls[1]).toContain("POST");
    // 3. Get parent SHA
    expect(ghCommandCalls[2]).toContain(`repos/sjawhar/legion/commits/${mergeOid}`);
    // 4. Get parent tree
    expect(ghCommandCalls[3]).toContain("repos/sjawhar/legion/git/commits/parent-sha-111");
    // 5. Create revert commit
    expect(ghCommandCalls[4]).toContain("repos/sjawhar/legion/git/commits");
    expect(ghCommandCalls[4]).toContain(`tree=tree-sha-222`);
    // 6. Update branch ref
    expect(ghCommandCalls[5]).toContain("PATCH");
    expect(ghCommandCalls[5]).toContain("sha=revert-sha-333");
  });

  it("cleans up branch on failure", async () => {
    const mergeOid = "merge-sha";
    ghCommandResponses = [
      // Get main SHA (must match mergeOid for guard)
      `${mergeOid}\n`,
      // Create branch ref
      "{}",
      // Get merge commit parents — fails
      new Error("API error"),
      // Branch cleanup
      "{}",
    ];

    await expect(
      createRevertCommit(
        "sjawhar/legion",
        {
          number: 100,
          title: "test",
          mergeCommit: { oid: mergeOid },
          headRefName: "branch",
        },
        42,
        "sjawhar-legion-42"
      )
    ).rejects.toThrow("Failed to create revert commit");

    // Verify cleanup DELETE was called
    const deleteCall = ghCommandCalls.find((c) => c.includes("-X") && c.includes("DELETE"));
    expect(deleteCall).toBeDefined();
  });

  it("throws when main has advanced past merge commit", async () => {
    ghCommandResponses = [
      // Get main SHA (different from merge commit)
      "advanced-main-sha\n",
    ];

    await expect(
      createRevertCommit(
        "sjawhar/legion",
        {
          number: 100,
          title: "test",
          mergeCommit: { oid: "original-merge-sha" },
          headRefName: "branch",
        },
        42,
        "sjawhar-legion-42"
      )
    ).rejects.toThrow("Main has advanced past merge commit");

    // Should only have made 1 call (get main SHA) — no branch created
    expect(ghCommandCalls).toHaveLength(1);
  });
});

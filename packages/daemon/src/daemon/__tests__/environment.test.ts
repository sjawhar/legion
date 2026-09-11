import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { execFileSync } from "node:child_process";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { CommandRunner } from "../../state/fetch";
import {
  createDaemonRunner,
  legionCliLauncherScript,
  type ResolveDaemonEnvironmentDeps,
  resolveDaemonEnvironment,
} from "../environment";

const OMP_PIN = "github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841";
const DAEMON_CLI_ENTRYPOINT = path.resolve(import.meta.dir, "../../cli/index.ts");

let stateDir: string;

beforeEach(async () => {
  stateDir = await mkdtemp(path.join(os.tmpdir(), "legion-environment-"));
});

afterEach(async () => {
  await rm(stateDir, { recursive: true, force: true });
});

function dependencies(
  overrides: Partial<ResolveDaemonEnvironmentDeps> = {}
): ResolveDaemonEnvironmentDeps {
  const legionBinDir = path.join(stateDir, "bin");
  const paneSearchPath = `${legionBinDir}${path.delimiter}/full/bin:/usr/bin`;
  return {
    env: { PATH: "/narrow/bin" },
    resolveExecutable(command, searchPath) {
      const paths: Record<string, string> = {
        "mise|/narrow/bin": "/tools/mise",
        [`jj|${paneSearchPath}`]: "/tools/jj",
        [`git|${paneSearchPath}`]: "/tools/git",
        [`gh|${paneSearchPath}`]: "/tools/gh",
        [`tmux|${paneSearchPath}`]: "/tools/tmux",
        "/mise/omp/bin/omp|": "/mise/omp/bin/omp",
      };
      return paths[`${command}|${searchPath ?? ""}`];
    },
    run: async (command) => {
      if (command.join(" ") === "/tools/mise env --json") {
        return {
          stdout: JSON.stringify({
            PATH: "/full/bin:/usr/bin",
            HOME: "/home/legion",
          }),
          stderr: "",
          exitCode: 0,
        };
      }
      if (command.join(" ") === `/tools/mise where ${OMP_PIN}`) {
        return { stdout: "/mise/omp\n", stderr: "", exitCode: 0 };
      }
      throw new Error(`Unexpected startup command: ${command.join(" ")}`);
    },
    stateDir,
    ...overrides,
  };
}

describe("legionCliLauncherScript", () => {
  it("re-execs bun against the resolved source entry when running cli/index.ts under bun", () => {
    const script = legionCliLauncherScript(
      "/opt/homebrew/bin/bun",
      "/repo/packages/daemon/src/cli/index.ts",
      undefined
    );
    expect(script).toBe(
      '#!/bin/sh\nexec "/opt/homebrew/bin/bun" "/repo/packages/daemon/src/cli/index.ts" "$@"\n'
    );
  });

  it("re-execs the runtime directly when Bun.main equals process.execPath (compiled binary)", () => {
    const script = legionCliLauncherScript(
      "/opt/legion/legion",
      "/opt/legion/packages/daemon/src/cli/index.ts",
      "/opt/legion/legion"
    );
    expect(script).toBe('#!/bin/sh\nexec "/opt/legion/legion" "$@"\n');
  });

  it("re-execs the runtime directly when argv1 has no .ts entry", () => {
    expect(legionCliLauncherScript("/opt/legion/legion", undefined, undefined)).toBe(
      '#!/bin/sh\nexec "/opt/legion/legion" "$@"\n'
    );
    expect(
      legionCliLauncherScript("/opt/legion/legion", "/opt/legion/dist/index.js", undefined)
    ).toBe('#!/bin/sh\nexec "/opt/legion/legion" "$@"\n');
  });

  it("refuses a relative execPath", () => {
    expect(() => legionCliLauncherScript("bun", undefined, undefined)).toThrow(
      "[legion] process.execPath must be an absolute path to build the legion CLI launcher"
    );
  });
});

describe("resolveDaemonEnvironment", () => {
  it("resolves tools and the pinned OMP binary from mise's full environment", async () => {
    const environment = await resolveDaemonEnvironment(`mise x ${OMP_PIN} -- omp`, dependencies());

    expect(environment).toMatchObject({
      commands: {
        jj: "/tools/jj",
        git: "/tools/git",
        gh: "/tools/gh",
        tmux: "/tools/tmux",
      },
      ompInvocation: "/mise/omp/bin/omp",
      paneEnv: {
        PATH: `${path.join(stateDir, "bin")}${path.delimiter}/full/bin:/usr/bin`,
        HOME: "/home/legion",
      },
    });
  });

  it("installs the legion CLI launcher and prepends its directory to paneEnv.PATH for every pane", async () => {
    const environment = await resolveDaemonEnvironment(`mise x ${OMP_PIN} -- omp`, dependencies());

    const legionBinDir = path.join(stateDir, "bin");
    expect(environment.paneEnv.PATH.startsWith(`${legionBinDir}${path.delimiter}`)).toBe(true);

    const launcherPath = path.join(legionBinDir, "legion");
    const launcherStat = await stat(launcherPath);
    expect(launcherStat.mode & 0o777).toBe(0o755);

    const content = await readFile(launcherPath, "utf8");
    expect(content).toBe(legionCliLauncherScript(process.execPath, process.argv[1], Bun.main));
  });

  it("writes a source-entry launcher that actually re-execs the real CLI (--help smoke test)", async () => {
    const originalArgv1 = process.argv[1];
    process.argv[1] = DAEMON_CLI_ENTRYPOINT;
    try {
      await resolveDaemonEnvironment(`mise x ${OMP_PIN} -- omp`, dependencies());
      const legionBinDir = path.join(stateDir, "bin");
      const content = await readFile(path.join(legionBinDir, "legion"), "utf8");
      expect(content).toBe(
        `#!/bin/sh\nexec "${process.execPath}" "${DAEMON_CLI_ENTRYPOINT}" "$@"\n`
      );

      const output = execFileSync(path.join(legionBinDir, "legion"), ["--help"], {
        encoding: "utf8",
      });
      expect(output).toContain("Wake-driven Legion daemon (legion)");
      expect(output).toContain("worker-shim");
    } finally {
      process.argv[1] = originalArgv1;
    }
  });

  it("strips DISPATCH_TOKEN, DISPATCH_URL, and DISPATCH_MCP_URL from paneEnv regardless of source", async () => {
    const environment = await resolveDaemonEnvironment(
      `mise x ${OMP_PIN} -- omp`,
      dependencies({
        env: {
          PATH: "/narrow/bin",
          DISPATCH_TOKEN: "leaked-from-daemon-process",
          DISPATCH_URL: "http://leaked-from-daemon-process",
        },
        run: async (command) => {
          if (command.join(" ") === "/tools/mise env --json") {
            return {
              stdout: JSON.stringify({
                PATH: "/full/bin:/usr/bin",
                HOME: "/home/legion",
                DISPATCH_TOKEN: "leaked-from-mise",
                DISPATCH_URL: "http://leaked-from-mise",
                DISPATCH_MCP_URL: "http://leaked-from-mise/mcp",
              }),
              stderr: "",
              exitCode: 0,
            };
          }
          if (command.join(" ") === `/tools/mise where ${OMP_PIN}`) {
            return { stdout: "/mise/omp\n", stderr: "", exitCode: 0 };
          }
          throw new Error(`Unexpected startup command: ${command.join(" ")}`);
        },
      })
    );

    expect(environment.paneEnv).not.toHaveProperty("DISPATCH_TOKEN");
    expect(environment.paneEnv).not.toHaveProperty("DISPATCH_URL");
    expect(environment.paneEnv).not.toHaveProperty("DISPATCH_MCP_URL");
    expect(environment.paneEnv).toMatchObject({
      PATH: `${path.join(stateDir, "bin")}${path.delimiter}/full/bin:/usr/bin`,
      HOME: "/home/legion",
    });

    const received: Array<{ options: Parameters<CommandRunner>[1] }> = [];
    const runner = createDaemonRunner(environment, async (_command, options) => {
      received.push({ options });
      return { stdout: "", stderr: "", exitCode: 0 };
    });
    await runner(["tmux", "new-session"]);

    expect(received[0]?.options?.env).not.toHaveProperty("DISPATCH_TOKEN");
    expect(received[0]?.options?.env).not.toHaveProperty("DISPATCH_URL");
    expect(received[0]?.options?.env).not.toHaveProperty("DISPATCH_MCP_URL");
  });

  it("strips dispatch env keys from the bootstrap `mise env`/`mise where` calls that predate createDaemonRunner", async () => {
    const received: Array<{ command: string[]; options?: Parameters<CommandRunner>[1] }> = [];
    const environment = await resolveDaemonEnvironment(
      `mise x ${OMP_PIN} -- omp`,
      dependencies({
        env: {
          PATH: "/narrow/bin",
          DISPATCH_TOKEN: "leaked-bootstrap-token",
          DISPATCH_URL: "http://leaked-bootstrap",
          DISPATCH_MCP_URL: "http://leaked-bootstrap/mcp",
        },
        run: async (command, options) => {
          received.push({ command, options });
          if (command.join(" ") === "/tools/mise env --json") {
            return {
              stdout: JSON.stringify({ PATH: "/full/bin:/usr/bin", HOME: "/home/legion" }),
              stderr: "",
              exitCode: 0,
            };
          }
          if (command.join(" ") === `/tools/mise where ${OMP_PIN}`) {
            return { stdout: "/mise/omp\n", stderr: "", exitCode: 0 };
          }
          throw new Error(`Unexpected startup command: ${command.join(" ")}`);
        },
      })
    );

    expect(environment.paneEnv).not.toHaveProperty("DISPATCH_TOKEN");

    const miseEnvCall = received.find(
      (call) => call.command.join(" ") === "/tools/mise env --json"
    );
    const miseWhereCall = received.find(
      (call) => call.command.join(" ") === `/tools/mise where ${OMP_PIN}`
    );
    // Neither bootstrap call inherits the daemon's raw ambient process environment: each passes
    // an explicit `env` with the dispatch keys already gone, even though `deps.env` carries them.
    // The `mise where` call runs after the legion CLI launcher is installed, so it sees the same
    // legion-bin-dir-prefixed PATH every pane gets.
    expect(miseEnvCall?.options?.env).toEqual({ PATH: "/narrow/bin" });
    expect(miseWhereCall?.options?.env).toEqual({
      PATH: `${path.join(stateDir, "bin")}${path.delimiter}/full/bin:/usr/bin`,
      HOME: "/home/legion",
    });
  });

  it("refuses unpinned OMP invocations without an explicit executable override", async () => {
    await expect(resolveDaemonEnvironment("omp", dependencies())).rejects.toThrow(
      "[legion] OMP invocation must be 'mise x <tool> -- omp'. Set LEGION_OMP_PATH to an absolute executable path."
    );
  });

  it("runs daemon subprocesses by absolute path with the restored full environment", async () => {
    const environment = await resolveDaemonEnvironment(`mise x ${OMP_PIN} -- omp`, dependencies());
    const received: Array<{
      command: string[];
      options: Parameters<CommandRunner>[1];
    }> = [];
    const runner = createDaemonRunner(environment, async (command, options) => {
      received.push({ command, options: options ?? {} });
      return { stdout: "", stderr: "", exitCode: 0 };
    });

    await runner(["jj", "status"]);
    await runner(["git", "status"], { env: { CUSTOM: "1" } });

    expect(received[0]?.command).toEqual(["/tools/jj", "status"]);
    expect(received[0]?.options?.env).toMatchObject({ HOME: "/home/legion" });
    expect(received[1]?.command).toEqual(["/tools/git", "status"]);
    expect(received[1]?.options?.env).toMatchObject({ HOME: "/home/legion", CUSTOM: "1" });
  });

  it("refuses startup with clear override guidance when a required tool is absent", async () => {
    const deps = dependencies({
      resolveExecutable(command) {
        if (command === "mise") return "/tools/mise";
        if (command === "git") return "/tools/git";
        if (command === "gh") return "/tools/gh";
        if (command === "tmux") return "/tools/tmux";
        return undefined;
      },
    });

    await expect(resolveDaemonEnvironment(`mise x ${OMP_PIN} -- omp`, deps)).rejects.toThrow(
      "[legion] Missing required daemon tools: jj (set LEGION_JJ_PATH to an absolute executable path)"
    );
  });
});

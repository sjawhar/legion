import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { execFileSync } from "node:child_process";
import { mkdir, mkdtemp, readdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import type { CommandRunner } from "../../state/fetch";
import {
  createDaemonRunner,
  isSecretLikeName,
  legionCliLauncherScript,
  PANE_ENV_ALLOW_LIST,
  type ResolveDaemonEnvironmentDeps,
  ROLE_PROMPT_FILES,
  resolveDaemonEnvironment,
  SOURCE_ROLE_PROMPTS_DIR,
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
      // No `mise where`: the daemon never resolves the pin to an install path.
      throw new Error(`Unexpected startup command: ${command.join(" ")}`);
    },
    stateDir,
    runtime: "tmux",
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
  it("resolves tools from mise's full environment and keeps the pinned OMP invocation as `mise x`, never the install path", async () => {
    const environment = await resolveDaemonEnvironment(`mise x ${OMP_PIN} -- omp`, dependencies());

    expect(environment).toMatchObject({
      commands: {
        jj: "/tools/jj",
        git: "/tools/git",
        gh: "/tools/gh",
        tmux: "/tools/tmux",
      },
      // Every pane and boot probe runs this fragment; mise activates the pinned tool inside the
      // pane. `mise` is the absolute path resolved on the daemon's own PATH.
      ompInvocation: `/tools/mise x ${OMP_PIN} -- omp`,
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

  it("builds paneEnv from the allow-list: nothing else from the daemon's environment reaches a pane", async () => {
    // The spec's scenario: a daemon launched with the two GitHub App private keys, listener and
    // Dispatch secrets, a provider key, an arbitrary FOO_SECRET, and the operator session's own
    // state (its secretsd token file, a pane's boot-token/grant pointers, JJ_CONFIG overlay,
    // OMP_SESSION_ID, outer tmux, SSH agent, OMP tuning, and a worker pane's six commit-identity
    // variables) in its environment.
    const environment = await resolveDaemonEnvironment(
      `mise x ${OMP_PIN} -- omp`,
      dependencies({
        env: {
          PATH: "/narrow/bin",
          HOME: "/home/legion",
          USER: "legion",
          LANG: "en_US.UTF-8",
          TERM: "tmux-256color",
          OMP_PROFILE: "legion",
          XDG_STATE_HOME: "/scratch/xdg-state",
          TMUX_TMPDIR: "/home/legion/.tmux/sockets",
          MISE_DATA_DIR: "/home/legion/.mise",
          GH_AGENT_APP_PRIVATE_KEY_B64: "leaked-agent-key",
          GH_REVIEW_APP_PRIVATE_KEY_B64: "leaked-review-key",
          ENVOY_GITHUB_WEBHOOK_SECRET: "leaked-webhook-secret",
          DISPATCH_TOKEN: "leaked-bearer",
          FOO_SECRET: "leaked-canary",
          ANTHROPIC_API_KEY: "leaked-provider-key",
          SECRETSD_SESSION_TOKEN_FILE: "/run/user/1000/secretsd/operator",
          LEGION_BOOT_TOKEN_FILE: "/leaked/legion-omp-legion-6-implementer",
          LEGION_GRANT_FILE: "/leaked/legion-omp-legion-6-implementer-grant",
          LEGION_DAEMON_URL: "http://127.0.0.1:13370",
          // a phase-worker pane's GitHub App commit identity (LEGION-44): set by that pane's own
          // `-e` pairs, never inherited — the root-architect and controller panes carry none
          JJ_USER: "legion-implementer[bot]",
          JJ_EMAIL: "1+legion-implementer[bot]@users.noreply.github.com",
          GIT_AUTHOR_NAME: "legion-implementer[bot]",
          GIT_AUTHOR_EMAIL: "1+legion-implementer[bot]@users.noreply.github.com",
          GIT_COMMITTER_NAME: "legion-implementer[bot]",
          GIT_COMMITTER_EMAIL: "1+legion-implementer[bot]@users.noreply.github.com",
          JJ_CONFIG:
            "/home/legion/.config/jj/config.toml:/home/legion/.cache/omp/jj/omp-attribution-x.toml",
          OMP_SESSION_ID: "operator-session",
          TMUX: "/tmp/tmux-1000/default,1,0",
          SSH_AUTH_SOCK: "/tmp/ssh-x/agent.1",
          MNEMOPI_VEC_WEIGHT: "0.5",
          ENVOY_TOKEN_FILE: "/leaked/legion-omp-legion-6-implementer-envoy_token",
        },
        run: async (command) => {
          if (command.join(" ") === "/tools/mise env --json") {
            return {
              stdout: JSON.stringify({
                PATH: "/full/bin:/usr/bin",
                CARGO_HOME: "/home/legion/.cargo",
                ENVOY_TOKEN: "leaked-from-mise",
              }),
              stderr: "",
              exitCode: 0,
            };
          }
          // No `mise where`: any other startup command is a regression.
          throw new Error(`Unexpected startup command: ${command.join(" ")}`);
        },
      })
    );

    // Exact equality: "nothing else" is the contract, not a list of names that must be absent.
    const expectedPaneEnv = {
      PATH: `${path.join(stateDir, "bin")}${path.delimiter}/full/bin:/usr/bin`,
      HOME: "/home/legion",
      USER: "legion",
      LANG: "en_US.UTF-8",
      TERM: "tmux-256color",
      OMP_PROFILE: "legion",
      XDG_STATE_HOME: "/scratch/xdg-state",
      TMUX_TMPDIR: "/home/legion/.tmux/sockets",
      MISE_DATA_DIR: "/home/legion/.mise",
      CARGO_HOME: "/home/legion/.cargo",
    };
    expect(environment.paneEnv).toEqual(expectedPaneEnv);

    // The private tmux server is forked by the daemon's first tmux command under this exact
    // environment, so this is what every pane inherits beneath its own `-e` pairs.
    const received: Array<{ options: Parameters<CommandRunner>[1] }> = [];
    const runner = createDaemonRunner(environment, async (_command, options) => {
      received.push({ options });
      return { stdout: "", stderr: "", exitCode: 0 };
    });
    await runner(["tmux", "new-session"]);
    expect(received[0]?.options?.env).toEqual(expectedPaneEnv);
  });

  it("drops a credential-shaped name even when mise emits it", async () => {
    const environment = await resolveDaemonEnvironment(
      `mise x ${OMP_PIN} -- omp`,
      dependencies({
        run: async (command) => {
          if (command.join(" ") === "/tools/mise env --json") {
            return {
              stdout: JSON.stringify({
                PATH: "/full/bin:/usr/bin",
                FOO_TOKEN: "x",
                BAR_SECRET: "y",
                BAZ_PRIVATE_KEY_B64: "z",
                QUX_TOKEN_FILE: "/f",
                KEEP_ME: "1",
              }),
              stderr: "",
              exitCode: 0,
            };
          }
          // No `mise where`: any other startup command is a regression.
          throw new Error(`Unexpected startup command: ${command.join(" ")}`);
        },
      })
    );

    expect(environment.paneEnv).toEqual({
      PATH: `${path.join(stateDir, "bin")}${path.delimiter}/full/bin:/usr/bin`,
      KEEP_ME: "1",
    });
  });

  it("honours LEGION_OMP_PATH from the daemon's own environment although panes never inherit it", async () => {
    const received: string[][] = [];
    const environment = await resolveDaemonEnvironment(
      `mise x ${OMP_PIN} -- omp`,
      dependencies({
        env: { PATH: "/narrow/bin", LEGION_OMP_PATH: "/opt/omp/bin/omp" },
        resolveExecutable(command, searchPath) {
          if (command === "mise") return "/tools/mise";
          if (command === "/opt/omp/bin/omp") return "/opt/omp/bin/omp";
          return searchPath ? `/tools/${command}` : undefined;
        },
        run: async (command) => {
          received.push(command);
          if (command.join(" ") === "/tools/mise env --json") {
            return {
              stdout: JSON.stringify({ PATH: "/full/bin:/usr/bin" }),
              stderr: "",
              exitCode: 0,
            };
          }
          throw new Error(`Unexpected startup command: ${command.join(" ")}`);
        },
      })
    );

    expect(environment).toMatchObject({ runtime: "tmux", ompInvocation: "/opt/omp/bin/omp" });
    expect(environment.paneEnv).not.toHaveProperty("LEGION_OMP_PATH");
    expect(received.map((command) => command.join(" "))).toEqual(["/tools/mise env --json"]);
  });

  it("strips every inherited worker-bin entry from the pane base PATH, so the daemon's own gh is never the shim", async () => {
    // A daemon started from inside a Legion pane: `mise env` keeps that pane's PATH head, which is
    // the pane's own `<state_dir>/worker-bin` (the `gh` shim), and may carry another daemon's
    // worker-bin further down. Both must go: the daemon's tool resolution and every pane it
    // launches run over this PATH, and `credentialProcessEnvironment` prepends worker-bin itself.
    const inheritedWorkerBin = path.join(stateDir, "worker-bin");
    const otherWorkerBin = "/other/state/worker-bin";
    const legionBinDir = path.join(stateDir, "bin");
    const cleanSearchPath = `${legionBinDir}${path.delimiter}/full/bin:/usr/bin`;
    const resolved: Array<{ command: string; searchPath: string | undefined }> = [];
    const environment = await resolveDaemonEnvironment(
      `mise x ${OMP_PIN} -- omp`,
      dependencies({
        env: { PATH: "/narrow/bin" },
        resolveExecutable(command, searchPath) {
          resolved.push({ command, searchPath });
          if (command === "mise") return "/tools/mise";
          // The shim would be found first by any resolver honoring an unstripped PATH.
          if (searchPath?.split(path.delimiter).includes(inheritedWorkerBin)) {
            return path.join(inheritedWorkerBin, command);
          }
          return `/tools/${command}`;
        },
        run: async (command) => {
          if (command.join(" ") === "/tools/mise env --json") {
            return {
              stdout: JSON.stringify({
                PATH: `${inheritedWorkerBin}:/full/bin:${otherWorkerBin}:/usr/bin`,
                HOME: "/home/legion",
              }),
              stderr: "",
              exitCode: 0,
            };
          }
          // No `mise where`: any other startup command is a regression.
          throw new Error(`Unexpected startup command: ${command.join(" ")}`);
        },
      })
    );

    expect(environment.paneEnv.PATH).toBe(cleanSearchPath);
    expect(environment.commands.gh).toBe("/tools/gh");
    for (const entry of resolved.filter((r) => r.command === "gh")) {
      expect(entry.searchPath).toBe(cleanSearchPath);
    }
  });

  it("gives the bootstrap `mise env` call only the allow-listed daemon environment, and runs no other startup command", async () => {
    const received: Array<{ command: string[]; options?: Parameters<CommandRunner>[1] }> = [];
    const environment = await resolveDaemonEnvironment(
      `mise x ${OMP_PIN} -- omp`,
      dependencies({
        env: {
          PATH: "/narrow/bin",
          HOME: "/home/legion",
          MISE_DATA_DIR: "/home/legion/.mise",
          GH_AGENT_APP_PRIVATE_KEY_B64: "leaked-agent-key",
          DISPATCH_TOKEN: "leaked-bootstrap-token",
          FOO_SECRET: "leaked-canary",
          MNEMOPI_VEC_WEIGHT: "0.5",
        },
        run: async (command, options) => {
          received.push({ command, options });
          if (command.join(" ") === "/tools/mise env --json") {
            return {
              stdout: JSON.stringify({ PATH: "/full/bin:/usr/bin", CARGO_HOME: "/cargo" }),
              stderr: "",
              exitCode: 0,
            };
          }
          throw new Error(`Unexpected startup command: ${command.join(" ")}`);
        },
      })
    );

    // The only bootstrap command is `mise env --json`, and it does not run under the daemon's raw
    // process environment: it gets exactly the allow-listed subset of `deps.env` (it predates
    // `paneEnv`). `mise where` is gone — the daemon hands panes the `mise x` invocation itself.
    expect(received.map((call) => call.command.join(" "))).toEqual(["/tools/mise env --json"]);
    expect(received[0]?.options?.env).toEqual({
      PATH: "/narrow/bin",
      HOME: "/home/legion",
      MISE_DATA_DIR: "/home/legion/.mise",
    });
    expect(environment.paneEnv).toEqual({
      PATH: `${path.join(stateDir, "bin")}${path.delimiter}/full/bin:/usr/bin`,
      HOME: "/home/legion",
      MISE_DATA_DIR: "/home/legion/.mise",
      CARGO_HOME: "/cargo",
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

  describe("runtime: kubernetes", () => {
    /** The worker image's PATH (`IMAGE_PATH`, k8s-manifests.ts): no mise, no tmux, no OMP on it
     * that the daemon would probe — the pod's `legion start` resolves only jj, git, and gh. */
    const IMAGE_PATH = "/opt/legion/bin:/opt/omp/bin:/usr/local/bin:/usr/bin:/bin";
    const kubernetesDependencies = (
      tools: Record<string, string>,
      overrides: Partial<ResolveDaemonEnvironmentDeps> = {}
    ): ResolveDaemonEnvironmentDeps => ({
      env: { PATH: IMAGE_PATH, HOME: "/home/legion", DISPATCH_TOKEN: "leaked" },
      resolveExecutable(command, searchPath) {
        // Every lookup must go through the pane PATH: `<stateDir>/bin` first, then the image's.
        expect(searchPath).toBe(`${path.join(stateDir, "bin")}${path.delimiter}${IMAGE_PATH}`);
        return tools[command];
      },
      run: async (command) => {
        throw new Error(`kubernetes mode must run no startup command (ran ${command.join(" ")})`);
      },
      stateDir,
      runtime: "kubernetes",
      ...overrides,
    });
    const imageTools = { jj: "/usr/local/bin/jj", git: "/usr/bin/git", gh: "/usr/local/bin/gh" };

    it("resolves jj, git, and gh from the process PATH with no mise, no tmux, and no OMP invocation", async () => {
      const environment = await resolveDaemonEnvironment(
        "mise x omp@1 -- omp",
        kubernetesDependencies(imageTools)
      );
      expect(environment).toEqual({
        runtime: "kubernetes",
        commands: imageTools,
        paneEnv: {
          PATH: `${path.join(stateDir, "bin")}${path.delimiter}${IMAGE_PATH}`,
          HOME: "/home/legion",
        },
        rolePromptsDir: SOURCE_ROLE_PROMPTS_DIR,
      });
      expect("ompInvocation" in environment).toBe(false);
      expect("tmux" in environment.commands).toBe(false);
    });

    it("installs the legion CLI launcher under <stateDir>/bin exactly as tmux mode does", async () => {
      await resolveDaemonEnvironment("mise x omp@1 -- omp", kubernetesDependencies(imageTools));
      const launcherPath = path.join(stateDir, "bin", "legion");
      expect((await stat(launcherPath)).mode & 0o777).toBe(0o755);
      expect(await readFile(launcherPath, "utf8")).toBe(
        legionCliLauncherScript(process.execPath, process.argv[1], Bun.main)
      );
    });

    it("strips an inherited worker-bin entry from the pane PATH", async () => {
      const inherited = `${path.join(stateDir, "worker-bin")}${path.delimiter}${IMAGE_PATH}`;
      const environment = await resolveDaemonEnvironment(
        "mise x omp@1 -- omp",
        kubernetesDependencies(imageTools, {
          env: { PATH: inherited, HOME: "/home/legion" },
        })
      );
      expect(environment.paneEnv.PATH).toBe(
        `${path.join(stateDir, "bin")}${path.delimiter}${IMAGE_PATH}`
      );
    });

    it("refuses startup naming the missing tool and its override, and never asks for mise or tmux", async () => {
      await expect(
        resolveDaemonEnvironment(
          "mise x omp@1 -- omp",
          kubernetesDependencies({ jj: "/usr/local/bin/jj", git: "/usr/bin/git" })
        )
      ).rejects.toThrow(
        "[legion] Missing required daemon tools: gh (set LEGION_GH_PATH to an absolute executable path)"
      );
    });

    it("runs daemon subprocesses by absolute path with the pane environment", async () => {
      const environment = await resolveDaemonEnvironment(
        "mise x omp@1 -- omp",
        kubernetesDependencies(imageTools)
      );
      const received: Array<{ command: string[]; options: Parameters<CommandRunner>[1] }> = [];
      const runner = createDaemonRunner(environment, async (command, options) => {
        received.push({ command, options: options ?? {} });
        return { stdout: "", stderr: "", exitCode: 0 };
      });
      await runner(["gh", "api", "user"]);
      expect(received[0]?.command).toEqual(["/usr/local/bin/gh", "api", "user"]);
      expect(received[0]?.options?.env).toMatchObject({ HOME: "/home/legion" });
      expect(received[0]?.options?.env).not.toHaveProperty("DISPATCH_TOKEN");
    });
  });

  describe("role prompts", () => {
    it("names the checkout's pi-envoy/roles by default, for both runtimes", async () => {
      const tmux = await resolveDaemonEnvironment(`mise x ${OMP_PIN} -- omp`, dependencies());
      expect(tmux.rolePromptsDir).toBe(SOURCE_ROLE_PROMPTS_DIR);
      for (const file of ROLE_PROMPT_FILES) {
        expect((await stat(path.join(SOURCE_ROLE_PROMPTS_DIR, file))).isFile()).toBe(true);
      }
    });

    it("boot-validates every prompt part the roles directory ships — a part added to the tree but not to the list is a spawn-time ENOENT on a pre-change LEGION_ROLE_PROMPTS_DIR", async () => {
      const shipped = (await readdir(SOURCE_ROLE_PROMPTS_DIR, { recursive: true }))
        .map(String)
        .filter((entry) => entry.endsWith(".md"))
        .sort();
      expect([...ROLE_PROMPT_FILES].sort()).toEqual(shipped);
    });

    it("takes LEGION_ROLE_PROMPTS_DIR from the daemon's own environment when it holds every prompt — the worker image's /opt/legion/roles", async () => {
      const promptsDir = path.join(stateDir, "roles");
      await mkdir(promptsDir);
      for (const file of ROLE_PROMPT_FILES) {
        const promptFile = path.join(promptsDir, file);
        await mkdir(path.dirname(promptFile), { recursive: true });
        await writeFile(promptFile, `# ${file}\n`);
      }
      const environment = await resolveDaemonEnvironment(
        `mise x ${OMP_PIN} -- omp`,
        dependencies({ env: { PATH: "/narrow/bin", LEGION_ROLE_PROMPTS_DIR: promptsDir } })
      );
      expect(environment.rolePromptsDir).toBe(promptsDir);
      // Daemon configuration, like LEGION_OMP_PATH: never part of what a pane inherits.
      expect(environment.paneEnv).not.toHaveProperty("LEGION_ROLE_PROMPTS_DIR");
    });

    it("refuses startup naming the directory, every missing prompt, and the override — never a spawn-time ENOENT", async () => {
      const promptsDir = path.join(stateDir, "roles");
      await mkdir(promptsDir);
      for (const file of ROLE_PROMPT_FILES) {
        if (file !== "architect-root.md" && file !== "tester.md") {
          const promptFile = path.join(promptsDir, file);
          await mkdir(path.dirname(promptFile), { recursive: true });
          await writeFile(promptFile, `# ${file}\n`);
        }
      }
      await expect(
        resolveDaemonEnvironment(
          `mise x ${OMP_PIN} -- omp`,
          dependencies({ env: { PATH: "/narrow/bin", LEGION_ROLE_PROMPTS_DIR: promptsDir } })
        )
      ).rejects.toThrow(
        `[legion] Role prompts directory ${promptsDir} is missing architect-root.md, tester.md (set LEGION_ROLE_PROMPTS_DIR to the directory holding pi-envoy's roles/*.md)`
      );
    });

    it("refuses a relative LEGION_ROLE_PROMPTS_DIR", async () => {
      await expect(
        resolveDaemonEnvironment(
          `mise x ${OMP_PIN} -- omp`,
          dependencies({ env: { PATH: "/narrow/bin", LEGION_ROLE_PROMPTS_DIR: "roles" } })
        )
      ).rejects.toThrow("[legion] LEGION_ROLE_PROMPTS_DIR must be an absolute path (got roles)");
    });
  });
});

describe("isSecretLikeName", () => {
  it("recognises a credential-shaped name by its trailing segment or a PRIVATE_KEY fragment, whatever its case", () => {
    for (const name of [
      "DISPATCH_TOKEN",
      "DISPATCH_TOKEN_FILE",
      "LEGION_BOOT_TOKEN_FILE",
      "LEGION_CONTROLLER_SECRET",
      "LEGION_GRANT",
      "LEGION_GRANT_FILE",
      "GH_TOKEN",
      "GITHUB_TOKEN",
      "ANTHROPIC_API_KEY",
      "ENVOY_TAILSCALE_OAUTH_CLIENT_KEY",
      "AWS_SECRET_ACCESS_KEY",
      "STARSHIP_SESSION_KEY",
      "GH_AGENT_APP_PRIVATE_KEY_B64",
      "ENVOY_GITHUB_WEBHOOK_SECRET",
      "DB_PASSWORD",
      "DB_PASSWD",
      "GITHUB_PAT",
      "GOOGLE_APPLICATION_CREDENTIALS",
      "GOOGLE_APPLICATION_CREDENTIALS_FILE",
      "npm_config_token",
      "my_api_key",
      "some_private_key_pem",
    ]) {
      expect(isSecretLikeName(name)).toBe(true);
    }
  });

  it("leaves ordinary names alone, including ones that merely contain a suffix mid-word", () => {
    // The segment must end the name (or be followed only by `_FILE`): `TOKENIZER`, `X_PATH`
    // (`_PAT` + `H`), `X_KEYBOARD` (`_KEY` + `BOARD`) are not credentials. Every allow-listed name
    // must land here, or the allow-list would hand a pane nothing.
    for (const name of [
      "PATH",
      "HOME",
      "TMUX_TMPDIR",
      "OMP_PROFILE",
      "SECRETSD_SOCK",
      "TOKENIZER",
      "MISE_DATA_DIR",
      "DISPATCH_URL",
      "LEGION_OMP_PATH",
      "XDG_KEYBOARD",
      "SSL_CERT_FILE",
      "NODE_EXTRA_CA_CERTS",
      "LEGION_CREDENTIAL_HELPER",
      "GH_CONFIG_DIR",
    ]) {
      expect(isSecretLikeName(name)).toBe(false);
    }
    for (const name of PANE_ENV_ALLOW_LIST) expect(isSecretLikeName(name)).toBe(false);
  });
});

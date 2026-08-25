import { describe, expect, it } from "bun:test";
import type { CommandRunner } from "../../state/fetch";
import {
  createDaemonRunner,
  type ResolveDaemonEnvironmentDeps,
  resolveDaemonEnvironment,
} from "../environment";

const OMP_PIN = "github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841";

function dependencies(
  overrides: Partial<ResolveDaemonEnvironmentDeps> = {}
): ResolveDaemonEnvironmentDeps {
  return {
    env: { PATH: "/narrow/bin" },
    resolveExecutable(command, searchPath) {
      const paths: Record<string, string> = {
        "mise|/narrow/bin": "/tools/mise",
        "jj|/full/bin:/usr/bin": "/tools/jj",
        "git|/full/bin:/usr/bin": "/tools/git",
        "gh|/full/bin:/usr/bin": "/tools/gh",
        "tmux|/full/bin:/usr/bin": "/tools/tmux",
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
    ...overrides,
  };
}

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
      paneEnv: { PATH: "/full/bin:/usr/bin", HOME: "/home/legion" },
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
      received.push({ command, options });
      return { stdout: "", stderr: "", exitCode: 0 };
    });

    await runner(["jj", "git", "clone"], {
      env: { GIT_CONFIG_COUNT: "1" },
      cwd: "/workspace",
    });

    expect(received).toEqual([
      {
        command: ["/tools/jj", "git", "clone"],
        options: {
          cwd: "/workspace",
          env: {
            PATH: "/full/bin:/usr/bin",
            HOME: "/home/legion",
            GIT_CONFIG_COUNT: "1",
          },
        },
      },
    ]);
  });

  it("refuses startup with clear override guidance when a required tool is absent", async () => {
    const deps = dependencies({
      resolveExecutable(command, searchPath) {
        if (command === "jj") return undefined;
        return dependencies().resolveExecutable?.(command, searchPath);
      },
    });

    await expect(resolveDaemonEnvironment(`mise x ${OMP_PIN} -- omp`, deps)).rejects.toThrow(
      "[legion] Missing required daemon tools: jj (set LEGION_JJ_PATH to an absolute executable path)"
    );
  });
});

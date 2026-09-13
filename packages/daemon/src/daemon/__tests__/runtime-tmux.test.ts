// `TmuxRuntime.scrubServerEnvironment`: what a boot against an already-running private server
// removes from that server's global and session environment tables before the first pane may open.
import { describe, expect, it } from "bun:test";
import { TmuxRuntime } from "../runtime-tmux";

interface Reply {
  stdout: string;
  stderr?: string;
  exitCode: number;
}

function runtimeOver(tables: { global: Reply; session: Reply }) {
  const commands: string[][] = [];
  const runtime = new TmuxRuntime({
    tmux: {
      socket: "legion-omp",
      run: async (cmd) => {
        commands.push(cmd);
        if (cmd[3] === "show-environment") return cmd[4] === "-g" ? tables.global : tables.session;
        return { stdout: "", stderr: "", exitCode: 0 };
      },
    },
    project: "omp",
    stateDir: "/unused",
    connectWorkerRpc: async () => {
      throw new Error("not exercised by this test");
    },
    workerRpcTimeoutMs: () => 5_000,
    now: () => 0,
    issueLocators: () => [],
    persist: async () => {},
  });
  return { runtime, commands };
}

const writes = (commands: string[][]) =>
  commands
    .filter((cmd) => cmd[3] === "set-environment" || cmd[3] === "set-option")
    .map((cmd) => cmd.slice(3));

const FRESH_SESSION_TABLE =
  "-DISPLAY\n-KRB5CCNAME\n-SSH_AGENT_PID\n-SSH_ASKPASS\n-SSH_AUTH_SOCK\n-SSH_CONNECTION\n-XAUTHORITY\n";

describe("TmuxRuntime.scrubServerEnvironment", () => {
  it("unsets every name the pane environment does not carry from both tables, and only those", async () => {
    const { runtime, commands } = runtimeOver({
      global: {
        stdout: [
          "GH_AGENT_APP_PRIVATE_KEY_B64=leaked-agent-key",
          "HOME=/home/legion",
          "PATH=/full/bin:/usr/bin",
          "DISPATCH_TOKEN=leaked-bearer",
          "-GH_REVIEW_APP_PRIVATE_KEY_B64",
          "PWD=/home/legion",
          "SHLVL=0",
          "",
        ].join("\n"),
        exitCode: 0,
      },
      // The operator attached once: tmux's default update-environment copied the client's SSH
      // agent socket and connection in; HOME arrived through some earlier `set-environment -t`.
      session: {
        stdout:
          "-DISPLAY\nHOME=/home/legion\nSSH_AUTH_SOCK=/tmp/ssh-x/agent.1\nSSH_CONNECTION=100.100.92.97 57158 100.113.243.90 22\n-XAUTHORITY\n",
        exitCode: 0,
      },
    });

    const removed = await runtime.scrubServerEnvironment({
      PATH: "/full/bin:/usr/bin",
      HOME: "/home/legion",
    });

    // Sorted and de-duplicated across both tables, so the boot log line is stable;
    // `-GH_REVIEW_…`/`-DISPLAY` are already unset and `PWD`/`SHLVL` are tmux's own.
    expect(removed).toEqual([
      "DISPATCH_TOKEN",
      "GH_AGENT_APP_PRIVATE_KEY_B64",
      "SSH_AUTH_SOCK",
      "SSH_CONNECTION",
    ]);
    expect(writes(commands)).toEqual([
      ["set-environment", "-g", "-u", "GH_AGENT_APP_PRIVATE_KEY_B64"],
      ["set-environment", "-g", "-u", "DISPATCH_TOKEN"],
      ["set-option", "-t", "legion-omp", "update-environment", ""],
      ["set-environment", "-t", "legion-omp", "-u", "SSH_AUTH_SOCK"],
      ["set-environment", "-t", "legion-omp", "-u", "SSH_CONNECTION"],
    ]);
  });

  it("only empties update-environment on a server this daemon forked itself, whose tables hold nothing to remove", async () => {
    const { runtime, commands } = runtimeOver({
      global: {
        stdout: "HOME=/home/legion\nPATH=/full/bin:/usr/bin\nPWD=/home/legion\nSHLVL=0\n",
        exitCode: 0,
      },
      session: { stdout: FRESH_SESSION_TABLE, exitCode: 0 },
    });

    const removed = await runtime.scrubServerEnvironment({
      PATH: "/full/bin:/usr/bin",
      HOME: "/home/legion",
    });

    expect(removed).toEqual([]);
    expect(writes(commands)).toEqual([
      ["set-option", "-t", "legion-omp", "update-environment", ""],
    ]);
  });

  it("touches nothing when no server is running yet", async () => {
    const gone = {
      stdout: "",
      stderr: "no server running on /tmp/tmux-1000/legion-omp",
      exitCode: 1,
    };
    const { runtime, commands } = runtimeOver({ global: gone, session: gone });

    expect(await runtime.scrubServerEnvironment({ PATH: "/full/bin" })).toEqual([]);
    expect(writes(commands)).toEqual([]);
  });

  it("scrubs the global table and skips the session table when the server is up without the daemon's session", async () => {
    const { runtime, commands } = runtimeOver({
      global: { stdout: "PATH=/full/bin\nFOO_SECRET=stale\n", exitCode: 0 },
      session: { stdout: "", stderr: "no such session: legion-omp", exitCode: 1 },
    });

    expect(await runtime.scrubServerEnvironment({ PATH: "/full/bin" })).toEqual(["FOO_SECRET"]);
    expect(writes(commands)).toEqual([["set-environment", "-g", "-u", "FOO_SECRET"]]);
  });
});

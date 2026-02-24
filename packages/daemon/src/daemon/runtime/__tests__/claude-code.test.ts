import { describe, expect, it } from "bun:test";
import { ClaudeCodeAdapter } from "../claude-code";

type SpawnResult = { exitCode: number | null; stdout?: string };

function makeSpawn(
  responses: Record<string, SpawnResult> = {},
  defaultResult: SpawnResult = { exitCode: 0, stdout: "" }
) {
  const calls: string[][] = [];
  const spawn = (cmd: string[]): SpawnResult => {
    calls.push(cmd);
    const key = cmd.join(" ");
    for (const [pattern, response] of Object.entries(responses)) {
      if (key.includes(pattern)) {
        return response;
      }
    }
    return defaultResult;
  };
  return { spawn, calls };
}

describe("ClaudeCodeAdapter", () => {
  describe("getPort", () => {
    it("returns 0 (no HTTP port for tmux-based runtime)", () => {
      const { spawn } = makeSpawn();
      const adapter = new ClaudeCodeAdapter("abc", spawn);
      expect(adapter.getPort()).toBe(0);
    });
  });

  describe("start", () => {
    it("creates a tmux session named legion-<shortId>", async () => {
      const { spawn, calls } = makeSpawn();
      const adapter = new ClaudeCodeAdapter("test123", spawn);
      await adapter.start({ workspace: "/tmp/test" });
      expect(calls[0]).toEqual(["tmux", "new-session", "-d", "-s", "legion-test123"]);
    });

    it("throws when tmux new-session fails", async () => {
      const { spawn } = makeSpawn({
        "new-session": { exitCode: 1, stdout: "" },
      });
      const adapter = new ClaudeCodeAdapter("fail", spawn);
      await expect(adapter.start({ workspace: "/tmp" })).rejects.toThrow(
        "Failed to create tmux session: legion-fail"
      );
    });

    it("forwards env vars to tmux session via set-environment", async () => {
      const { spawn, calls } = makeSpawn();
      const adapter = new ClaudeCodeAdapter("test", spawn);
      await adapter.start({
        workspace: "/tmp/ws",
        env: { LEGION_TEAM_ID: "abc", LEGION_DIR: "/test" },
      });
      const envCalls = calls.filter((c) => c[1] === "set-environment");
      expect(envCalls).toHaveLength(2);
      expect(envCalls[0]).toEqual([
        "tmux",
        "set-environment",
        "-t",
        "legion-test",
        "LEGION_TEAM_ID",
        "abc",
      ]);
      expect(envCalls[1]).toEqual([
        "tmux",
        "set-environment",
        "-t",
        "legion-test",
        "LEGION_DIR",
        "/test",
      ]);
    });

    it("does not call set-environment when opts.env is undefined", async () => {
      const { spawn, calls } = makeSpawn();
      const adapter = new ClaudeCodeAdapter("test", spawn);
      await adapter.start({ workspace: "/tmp/ws" });
      const envCalls = calls.filter((c) => c[1] === "set-environment");
      expect(envCalls).toHaveLength(0);
    });
  });

  describe("stop", () => {
    it("kills the tmux session", async () => {
      const { spawn, calls } = makeSpawn();
      const adapter = new ClaudeCodeAdapter("xyz", spawn);
      await adapter.stop();
      expect(calls[0]).toEqual(["tmux", "kill-session", "-t", "legion-xyz"]);
    });
  });

  describe("healthy", () => {
    it("returns true when tmux session exists", async () => {
      const { spawn } = makeSpawn({
        "has-session": { exitCode: 0, stdout: "" },
      });
      const adapter = new ClaudeCodeAdapter("h1", spawn);
      expect(await adapter.healthy()).toBe(true);
    });

    it("returns false when tmux session missing", async () => {
      const { spawn } = makeSpawn({
        "has-session": { exitCode: 1, stdout: "" },
      });
      const adapter = new ClaudeCodeAdapter("h2", spawn);
      expect(await adapter.healthy()).toBe(false);
    });
  });

  describe("createSession", () => {
    it("creates a tmux window and cds into workspace", async () => {
      const { spawn, calls } = makeSpawn();
      const adapter = new ClaudeCodeAdapter("cs1", spawn);
      const result = await adapter.createSession("ses_abc", "/home/work");
      expect(result).toBe("ses_abc");
      expect(calls[0]).toEqual(["tmux", "new-window", "-t", "legion-cs1", "-n", "ses_abc", "-d"]);
      expect(calls[1]).toEqual([
        "tmux",
        "send-keys",
        "-t",
        "legion-cs1:ses_abc",
        "cd '/home/work'",
        "Enter",
      ]);
    });
  });

  describe("sendPrompt", () => {
    it("launches fresh claude when no process running (none)", async () => {
      const { spawn, calls } = makeSpawn({
        pane_current_command: { exitCode: 1, stdout: "" },
      });
      const adapter = new ClaudeCodeAdapter("sp1", spawn);
      await adapter.sendPrompt("ses_1", "hello world");

      const sendKeysCall = calls.find(
        (c) => c.includes("send-keys") && c.some((arg) => arg.includes("claude"))
      );
      expect(sendKeysCall).toBeDefined();
      const cmdArg = sendKeysCall!.find((a) => a.includes("claude"));
      expect(cmdArg).toContain("--session-id");
      expect(cmdArg).toContain("ses_1");
      expect(cmdArg).toContain("--dangerously-skip-permissions");
      expect(cmdArg).toContain("hello world");
    });

    it("sends text directly when claude is running", async () => {
      const { spawn, calls } = makeSpawn({
        pane_current_command: { exitCode: 0, stdout: "claude" },
      });
      const adapter = new ClaudeCodeAdapter("sp2", spawn);
      await adapter.sendPrompt("ses_2", "do something");

      const sendKeysCall = calls.find((c) => c.includes("send-keys") && c.includes("do something"));
      expect(sendKeysCall).toBeDefined();
      expect(sendKeysCall!.join(" ")).not.toContain("--session-id");
    });

    it("resumes claude session when process exited", async () => {
      const callCount = { cmd: 0, dead: 0 };
      const spawn = (cmd: string[]): SpawnResult => {
        const key = cmd.join(" ");
        if (key.includes("pane_current_command")) {
          callCount.cmd++;
          return { exitCode: 0, stdout: "bash" };
        }
        if (key.includes("pane_dead")) {
          callCount.dead++;
          return { exitCode: 0, stdout: "1" };
        }
        return { exitCode: 0, stdout: "" };
      };

      const spawnCalls: string[][] = [];
      const wrappedSpawn = (cmd: string[]): SpawnResult => {
        spawnCalls.push(cmd);
        return spawn(cmd);
      };
      const adapter2 = new ClaudeCodeAdapter("sp3", wrappedSpawn);
      await adapter2.sendPrompt("ses_3", "continue work");

      const sendKeysCall = spawnCalls.find(
        (c) => c.includes("send-keys") && c.some((a) => a.includes("--resume"))
      );
      expect(sendKeysCall).toBeDefined();
      const cmdArg = sendKeysCall!.find((a) => a.includes("--resume"));
      expect(cmdArg).toContain("ses_3");
      expect(cmdArg).toContain("--dangerously-skip-permissions");
    });
  });

  describe("getSessionStatus", () => {
    it("returns running when claude process is alive", async () => {
      const { spawn } = makeSpawn({
        pane_current_command: { exitCode: 0, stdout: "claude" },
      });
      const adapter = new ClaudeCodeAdapter("gs1", spawn);
      const status = await adapter.getSessionStatus("ses_x");
      expect(status).toEqual({ data: { status: "running" } });
    });

    it("returns idle when no claude process", async () => {
      const { spawn } = makeSpawn({
        pane_current_command: { exitCode: 1, stdout: "" },
      });
      const adapter = new ClaudeCodeAdapter("gs2", spawn);
      const status = await adapter.getSessionStatus("ses_y");
      expect(status).toEqual({ data: { status: "idle" } });
    });
  });

  describe("shellEscape", () => {
    it("escapes single quotes in sendPrompt (none path)", async () => {
      const { spawn, calls } = makeSpawn({
        pane_current_command: { exitCode: 1, stdout: "" },
      });
      const adapter = new ClaudeCodeAdapter("t1", spawn);
      await adapter.sendPrompt("ses_1", "hello'; rm -rf /; echo '");
      const sendKeysCall = calls.find(
        (c) => c.includes("send-keys") && c.some((a) => a.includes("claude"))
      );
      expect(sendKeysCall).toBeDefined();
      const cmdArg = sendKeysCall!.find((a) => a.includes("claude"))!;
      expect(cmdArg).toContain("hello'\\''");
      expect(cmdArg).not.toMatch(/hello';/);
    });

    it("escapes single quotes in sendPrompt (exited path)", async () => {
      const callCount = { cmd: 0, dead: 0 };
      const spawn = (cmd: string[]): SpawnResult => {
        const key = cmd.join(" ");
        if (key.includes("pane_current_command")) {
          callCount.cmd++;
          return { exitCode: 0, stdout: "bash" };
        }
        if (key.includes("pane_dead")) {
          callCount.dead++;
          return { exitCode: 0, stdout: "1" };
        }
        return { exitCode: 0, stdout: "" };
      };
      const spawnCalls: string[][] = [];
      const wrappedSpawn = (cmd: string[]): SpawnResult => {
        spawnCalls.push(cmd);
        return spawn(cmd);
      };
      const adapter = new ClaudeCodeAdapter("t1e", wrappedSpawn);
      await adapter.sendPrompt("ses_1", "it's a test");
      const sendKeysCall = spawnCalls.find(
        (c) => c.includes("send-keys") && c.some((a) => a.includes("--resume"))
      );
      expect(sendKeysCall).toBeDefined();
      const cmdArg = sendKeysCall!.find((a) => a.includes("--resume"))!;
      expect(cmdArg).toContain("it'\\''s a test");
    });

    it("escapes single quotes in createSession workspace", async () => {
      const { spawn, calls } = makeSpawn();
      const adapter = new ClaudeCodeAdapter("t2", spawn);
      await adapter.createSession("ses_2", "/tmp/my dir/with'quote");
      const sendKeysCall = calls.find((c) => c.includes("send-keys"));
      const cdArg = sendKeysCall?.find((a) => a.startsWith("cd "));
      expect(cdArg).toBe("cd '/tmp/my dir/with'\\''quote'");
    });

    it("createSession throws on tmux new-window failure", async () => {
      const { spawn } = makeSpawn({
        "new-window": { exitCode: 1, stdout: "" },
      });
      const adapter = new ClaudeCodeAdapter("t3", spawn);
      await expect(adapter.createSession("ses_3", "/tmp/work")).rejects.toThrow(
        "Failed to create tmux window"
      );
    });

    it("start() sends cd command with escaped workspace", async () => {
      const { spawn, calls } = makeSpawn();
      const adapter = new ClaudeCodeAdapter("t4", spawn);
      await adapter.start({ workspace: "/tmp/my workspace" });
      const cdCall = calls.find(
        (c) => c.includes("send-keys") && c.some((a) => a.startsWith("cd "))
      );
      expect(cdCall).toBeDefined();
      expect(cdCall!.find((a) => a.startsWith("cd "))).toBe("cd '/tmp/my workspace'");
    });
  });

  describe("isProcessAlive exact match", () => {
    it("does not treat 'claudebot' as running claude", async () => {
      const { spawn } = makeSpawn({
        pane_current_command: { exitCode: 0, stdout: "claudebot" },
      });
      const adapter = new ClaudeCodeAdapter("ip1", spawn);
      const status = await adapter.getSessionStatus("ses_z");
      expect(status).toEqual({ data: { status: "idle" } });
    });

    it("treats '/usr/bin/claude' as running claude", async () => {
      const { spawn } = makeSpawn({
        pane_current_command: { exitCode: 0, stdout: "/usr/bin/claude" },
      });
      const adapter = new ClaudeCodeAdapter("ip2", spawn);
      const status = await adapter.getSessionStatus("ses_z");
      expect(status).toEqual({ data: { status: "running" } });
    });
  });
});

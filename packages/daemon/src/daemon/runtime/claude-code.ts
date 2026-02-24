import type { RuntimeAdapter, RuntimeStartOptions } from "./types";

type SpawnResult = {
  exitCode: number | null;
  stdout?: string;
};

export type SpawnFn = (cmd: string[]) => SpawnResult;


function shellEscape(s: string): string {
  return s.replace(/'/g, "'\\''");
}

// TECH DEBT: Using spawnSync blocks the event loop (~2ms per call).
// Acceptable for v1 but should migrate to async Bun.spawn if tmux
// operations become a bottleneck under high concurrency.

const defaultSpawn: SpawnFn = (cmd) => {
  const result = Bun.spawnSync(cmd, { stdio: ["ignore", "pipe", "pipe"] });
  return {
    exitCode: result.exitCode,
    stdout: result.stdout?.toString().trim(),
  };
};

export class ClaudeCodeAdapter implements RuntimeAdapter {
  private readonly sessionName: string;
  private readonly spawn: SpawnFn;

  constructor(shortId: string, spawn?: SpawnFn) {
    this.sessionName = `legion-${shortId}`;
    this.spawn = spawn ?? defaultSpawn;
  }

  getPort(): number {
    return 0;
  }

  async start(opts: RuntimeStartOptions): Promise<void> {
    const result = this.spawn(["tmux", "new-session", "-d", "-s", this.sessionName]);
    if (result.exitCode !== 0) {
      throw new Error(`Failed to create tmux session: ${this.sessionName}`);
    }
    if (opts.workspace) {
      this.spawn(["tmux", "send-keys", "-t", this.sessionName, `cd '${shellEscape(opts.workspace)}'`, "Enter"]);
    }
  }

  async stop(): Promise<void> {
    this.spawn(["tmux", "kill-session", "-t", this.sessionName]);
  }

  async healthy(): Promise<boolean> {
    const result = this.spawn(["tmux", "has-session", "-t", this.sessionName]);
    return result.exitCode === 0;
  }

  async createSession(sessionId: string, workspace: string): Promise<string> {
    const newWindowResult = this.spawn(["tmux", "new-window", "-t", this.sessionName, "-n", sessionId, "-d"]);
    if (newWindowResult.exitCode !== 0) {
      throw new Error(`Failed to create tmux window for session ${sessionId}`);
    }
    this.spawn([
      "tmux",
      "send-keys",
      "-t",
      `${this.sessionName}:${sessionId}`,
      `cd '${shellEscape(workspace)}'`,
      "Enter",
    ]);
    return sessionId;
  }

  async sendPrompt(sessionId: string, text: string): Promise<void> {
    const windowTarget = `${this.sessionName}:${sessionId}`;
    const alive = this.isProcessAlive(sessionId);

    if (alive === "running") {
      this.spawn(["tmux", "send-keys", "-t", windowTarget, text, "Enter"]);
    } else if (alive === "exited") {
      const cmd = `claude --resume '${shellEscape(sessionId)}' -p '${shellEscape(text)}' --dangerously-skip-permissions`;
      this.spawn(["tmux", "send-keys", "-t", windowTarget, cmd, "Enter"]);
    } else {
      const cmd = `claude -p '${shellEscape(text)}' --session-id '${shellEscape(sessionId)}' --dangerously-skip-permissions`;
      this.spawn(["tmux", "send-keys", "-t", windowTarget, cmd, "Enter"]);
    }
  }

  async getSessionStatus(sessionId: string): Promise<{ data?: unknown; error?: unknown }> {
    const alive = this.isProcessAlive(sessionId);
    return { data: { status: alive === "running" ? "running" : "idle" } };
  }

  private isProcessAlive(sessionId: string): "running" | "exited" | "none" {
    const windowTarget = `${this.sessionName}:${sessionId}`;
    const result = this.spawn([
      "tmux",
      "list-panes",
      "-t",
      windowTarget,
      "-F",
      "#{pane_current_command}",
    ]);
    if (result.exitCode !== 0) {
      return "none";
    }
    const cmd = result.stdout ?? "";
    if (cmd.trim() === "claude" || cmd.trim().endsWith("/claude")) {
      return "running";
    }
    const deadResult = this.spawn(["tmux", "list-panes", "-t", windowTarget, "-F", "#{pane_dead}"]);
    if (deadResult.exitCode !== 0) {
      return "none";
    }
    if (deadResult.stdout === "1") {
      return "exited";
    }
    return "none";
  }
}

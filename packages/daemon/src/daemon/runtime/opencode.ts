import {
  createSession,
  createWorkerClient,
  healthCheck,
  spawnSharedServe,
  stopServe,
  waitForHealthy,
} from "../serve-manager";
import type { RuntimeAdapter, RuntimeStartOptions } from "./types";

export class OpenCodeAdapter implements RuntimeAdapter {
  private pid = 0;
  private readonly workspaces = new Map<string, string>();

  constructor(private readonly port: number) {}

  getPort(): number {
    return this.port;
  }
  async start(opts: RuntimeStartOptions): Promise<void> {
    const serve = await spawnSharedServe({
      port: this.port,
      workspace: opts.workspace,
      logDir: opts.logDir,
      env: opts.env,
    });
    this.pid = serve.pid;
    await waitForHealthy(this.port);
  }

  async stop(): Promise<void> {
    if (this.pid > 0) {
      await stopServe(this.port, this.pid);
    }
  }

  async healthy(): Promise<boolean> {
    return healthCheck(this.port);
  }

  async createSession(sessionId: string, workspace: string): Promise<string> {
    const actualId = await createSession(this.port, sessionId, workspace);
    this.workspaces.set(actualId, workspace);
    return actualId;
  }

  async sendPrompt(sessionId: string, text: string): Promise<void> {
    const client = createWorkerClient(this.port, this.workspaces.get(sessionId) ?? "");
    await client.session.promptAsync({
      sessionID: sessionId,
      parts: [{ type: "text", text }],
    });
  }

  async getSessionStatus(sessionId: string): Promise<{ data?: unknown; error?: unknown }> {
    const client = createWorkerClient(this.port, this.workspaces.get(sessionId) ?? "");
    const result = await client.session.status();
    if (result.error || !result.data) {
      return result;
    }
    // session.status() returns a map of ALL sessions — filter to just the requested one
    const allStatuses = result.data as Record<string, unknown>;
    const sessionStatus = allStatuses[sessionId];
    return { data: sessionStatus ?? { type: "idle" } };
  }
}

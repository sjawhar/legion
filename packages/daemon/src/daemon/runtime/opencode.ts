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
  private subprocess: ReturnType<typeof Bun.spawn> | null = null;
  private readonly workspaces = new Map<string, string>();
  private lastStartOpts: RuntimeStartOptions | null = null;
  onServeExit: ((code: number | null) => void) | null = null;

  constructor(private readonly port: number) {}
  getPort(): number {
    return this.port;
  }
  async start(opts: RuntimeStartOptions): Promise<void> {
    this.lastStartOpts = opts;
    const serve = await spawnSharedServe({
      port: this.port,
      workspace: opts.workspace,
      logDir: opts.logDir,
      env: opts.env,
    });
    this.pid = serve.pid;
    this.subprocess = serve.subprocess;
    serve.subprocess.exited.then((code) => {
      console.error(`[daemon] shared serve exited unexpectedly: pid=${this.pid} code=${code}`);
      this.pid = 0;
      this.subprocess = null;
      this.onServeExit?.(code);
    });
    await waitForHealthy(this.port);
  }

  private async ensureRunning(): Promise<void> {
    if (await healthCheck(this.port)) return;
    if (!this.lastStartOpts) throw new Error("Shared serve not initialized");
    console.log("Shared serve not running, starting on demand...");
    await this.start(this.lastStartOpts);
  }

  async stop(): Promise<void> {
    if (this.subprocess) {
      this.subprocess.kill();
      await this.subprocess.exited;
      this.subprocess = null;
    } else if (this.pid > 0) {
      await stopServe(this.port, this.pid);
    }
    this.pid = 0;
  }

  async healthy(): Promise<boolean> {
    return healthCheck(this.port);
  }

  async createSession(sessionId: string, workspace: string): Promise<string> {
    await this.ensureRunning();
    const actualId = await createSession(this.port, sessionId, workspace);
    this.workspaces.set(actualId, workspace);
    return actualId;
  }

  async sendPrompt(sessionId: string, text: string): Promise<void> {
    await this.ensureRunning();
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

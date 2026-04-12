import {
  createSession,
  createWorkerClient,
  deleteSession,
  healthCheck,
  spawnSharedServe,
  stopServe,
  waitForHealthy,
} from "../serve-manager";
import type { RuntimeAdapter, RuntimeStartOptions } from "./types";

export class OpenCodeAdapter implements RuntimeAdapter {
  private pid = 0;
  private readonly workspaces = new Map<string, string>();
  private startOpts: RuntimeStartOptions | null = null;
  private startingPromise: Promise<void> | null = null;

  constructor(private readonly port: number) {}

  getPort(): number {
    return this.port;
  }

  getServePid(): number {
    return this.pid;
  }

  /** Store start opts for lazy serve startup via ensureRunning(). */
  configure(opts: RuntimeStartOptions): void {
    this.startOpts = opts;
  }

  async start(opts: RuntimeStartOptions): Promise<void> {
    this.startOpts = opts;
    const serve = await spawnSharedServe({
      port: this.port,
      workspace: opts.workspace,
      logDir: opts.logDir,
      env: opts.env,
    });
    this.pid = serve.pid;
    await waitForHealthy(this.port);
  }

  /** Start the serve on demand if it's not running. Coalesces concurrent calls. */
  async ensureRunning(): Promise<void> {
    if (await healthCheck(this.port)) return;
    if (!this.startOpts)
      throw new Error("Shared serve not configured \u2014 call configure() or start() first");
    if (!this.startingPromise) {
      this.startingPromise = this.start(this.startOpts).finally(() => {
        this.startingPromise = null;
      });
    }
    await this.startingPromise;
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
    await this.ensureRunning();
    const actualId = await createSession(this.port, sessionId, workspace);
    this.workspaces.set(actualId, workspace);
    return actualId;
  }

  async deleteSession(sessionId: string): Promise<void> {
    await deleteSession(this.port, sessionId);
    this.workspaces.delete(sessionId);
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
    const sessionStatus = (allStatuses[sessionId] ?? { type: "idle" }) as { type: string };

    // Enrich with activity signals from session data and messages
    try {
      const [sessionResult, messagesResult] = await Promise.all([
        client.session.get({ sessionID: sessionId }),
        client.session.messages({ sessionID: sessionId }),
      ]);

      let lastActivityAt: string | null = null;
      let messageCount = 0;
      let turnCount = 0;
      let tokensUsed = 0;

      const session = sessionResult.data as { time?: { updated?: number } } | undefined;
      if (session?.time?.updated) {
        lastActivityAt = new Date(session.time.updated * 1000).toISOString();
      }

      // session.messages() returns Array<{ info: Message; parts: Part[] }>
      const messages = messagesResult.data as
        | Array<{
            info: { role: string; tokens?: { total?: number; input: number; output: number } };
          }>
        | undefined;
      if (Array.isArray(messages)) {
        messageCount = messages.length;
        for (const { info: msg } of messages) {
          if (msg.role === "assistant" && msg.tokens) {
            turnCount++;
            tokensUsed += msg.tokens.total ?? msg.tokens.input + msg.tokens.output;
          } else if (msg.role === "assistant") {
            turnCount++;
          }
        }
      }

      return {
        data: {
          ...sessionStatus,
          lastActivityAt,
          messageCount,
          turnCount,
          phase: sessionStatus.type,
          tokensUsed,
        },
      };
    } catch {
      // If enrichment fails, return basic status — don't break existing behavior
      return { data: sessionStatus };
    }
  }

  async listActiveSessions(): Promise<Set<string>> {
    const client = createWorkerClient(this.port, "");
    const result = await client.session.status();
    if (result.error || !result.data) {
      throw new Error(`Failed to list sessions: ${JSON.stringify(result.error ?? "no data")}`);
    }
    return new Set(Object.keys(result.data as Record<string, unknown>));
  }
}

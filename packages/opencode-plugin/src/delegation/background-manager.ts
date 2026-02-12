import type { PluginInput } from "@opencode-ai/plugin";
import {
  registerSubagentSession,
  unregisterSubagentSession,
} from "../hooks/subagent-question-blocker";
import type { BackgroundTask, LaunchOptions } from "./types";

type OpencodeClient = PluginInput["client"];

function generateTaskId(): string {
  return `bg_${Math.random().toString(36).slice(2, 10)}`;
}

export class BackgroundTaskManager {
  private tasks = new Map<string, BackgroundTask>();
  private tasksBySessionId = new Map<string, string>();
  private client: OpencodeClient;
  private directory: string;

  constructor(ctx: PluginInput) {
    this.client = ctx.client;
    this.directory = ctx.directory;
  }

  /**
   * Launch a background task.
   * Creates session synchronously so sessionID is available immediately,
   * then starts the prompt in background.
   */
  async launch(opts: LaunchOptions): Promise<BackgroundTask> {
    const task: BackgroundTask = {
      id: generateTaskId(),
      status: "pending",
      agent: opts.agent,
      model: opts.model ?? "anthropic/claude-sonnet-4-20250514",
      description: opts.description,
      createdAt: Date.now(),
    };

    this.tasks.set(task.id, task);

    try {
      const session = await this.client.session.create({
        body: {
          parentID: opts.parentSessionId,
          title: `Background: ${opts.description}`,
        },
        query: { directory: this.directory },
      });

      if (!session.data?.id) {
        throw new Error("Failed to create background session");
      }

      task.sessionID = session.data.id;
      this.tasksBySessionId.set(session.data.id, task.id);
      registerSubagentSession(session.data.id);

      this.startPrompt(task, opts).catch(() => {});
    } catch (err) {
      task.status = "failed";
      task.error = err instanceof Error ? err.message : String(err);
      task.completedAt = Date.now();
    }

    return task;
  }

  private async startPrompt(task: BackgroundTask, opts: LaunchOptions): Promise<void> {
    try {
      if (task.status === "cancelled" || !task.sessionID) return;
      task.status = "running";

      const modelStr = task.model;
      const slashIdx = modelStr.indexOf("/");
      const providerID = slashIdx >= 0 ? modelStr.slice(0, slashIdx) : modelStr;
      const modelID = slashIdx >= 0 ? modelStr.slice(slashIdx + 1) : modelStr;

      await this.client.session.promptAsync({
        path: { id: task.sessionID },
        body: {
          agent: opts.agent,
          model: { providerID, modelID },
          parts: [{ type: "text" as const, text: opts.prompt }],
          ...(opts.systemPrompt ? { system: opts.systemPrompt } : {}),
        },
        query: { directory: this.directory },
      });
    } catch (err) {
      task.status = "failed";
      task.error = err instanceof Error ? err.message : String(err);
      task.completedAt = Date.now();
      if (task.sessionID) {
        this.tasksBySessionId.delete(task.sessionID);
        unregisterSubagentSession(task.sessionID);
      }
    }
  }

  getTask(id: string): BackgroundTask | undefined {
    return this.tasks.get(id);
  }

  async getTaskOutput(id: string): Promise<string> {
    const task = this.tasks.get(id);
    if (!task) return `Task not found: ${id}`;

    if (task.status === "pending" || task.status === "running") {
      const elapsed = Math.floor((Date.now() - task.createdAt) / 1000);
      return `Task ${id} still running (${elapsed}s elapsed)`;
    }

    if (task.status === "failed") return `Task failed: ${task.error}`;
    if (task.status === "cancelled") return "Task cancelled";
    if (task.result) return task.result;

    if (task.sessionID) {
      try {
        const messagesResult = await this.client.session.messages({
          path: { id: task.sessionID },
          query: { directory: this.directory },
        });
        const messages = (messagesResult.data ?? []) as Array<{
          info?: { role: string };
          parts?: Array<{ type: string; text?: string }>;
        }>;
        const assistantMessages = messages.filter((m) => m.info?.role === "assistant");

        const texts: string[] = [];
        for (const msg of assistantMessages) {
          for (const part of msg.parts ?? []) {
            if (part.type === "text" && part.text) {
              texts.push(part.text);
            }
          }
        }

        const output = texts.filter((t) => t.length > 0).join("\n\n");
        if (output) {
          task.result = output;
          return output;
        }
      } catch {
        // intentional fall-through
      }
    }

    return "No output available";
  }

  cancel(id: string): boolean {
    const task = this.tasks.get(id);
    if (!task || (task.status !== "running" && task.status !== "pending")) {
      return false;
    }
    task.status = "cancelled";
    task.completedAt = Date.now();
    if (task.sessionID) {
      this.tasksBySessionId.delete(task.sessionID);
      unregisterSubagentSession(task.sessionID);
      this.client.session
        .abort({
          path: { id: task.sessionID },
          query: { directory: this.directory },
        })
        .catch(() => {});
    }
    return true;
  }

  cancelAll(): number {
    let count = 0;
    for (const task of this.tasks.values()) {
      if ((task.status === "running" || task.status === "pending") && this.cancel(task.id)) {
        count++;
      }
    }
    return count;
  }

  /**
   * Handle session.status events for completion detection.
   * Wire this into the plugin's event handler.
   */
  handleSessionStatus(event: { type: string; properties?: Record<string, unknown> }): void {
    if (event.type !== "session.status") return;

    const sessionID = event.properties?.sessionID as string | undefined;
    if (!sessionID) return;

    const taskId = this.tasksBySessionId.get(sessionID);
    if (!taskId) return;

    const task = this.tasks.get(taskId);
    if (!task || (task.status !== "running" && task.status !== "pending")) return;

    const status = event.properties?.status as { type: string } | undefined;
    if (status?.type === "idle") {
      task.status = "completed";
      task.completedAt = Date.now();
      unregisterSubagentSession(sessionID);
    } else if (status?.type === "error") {
      task.status = "failed";
      task.error = "Session entered error state";
      task.completedAt = Date.now();
      unregisterSubagentSession(sessionID);
    }
  }

  /**
   * Clean up task data when a session is deleted.
   * Prevents memory leaks from accumulated completed tasks.
   */
  isBackgroundSession(sessionID: string): boolean {
    return this.tasksBySessionId.has(sessionID);
  }

  cleanup(sessionID: string): void {
    const taskId = this.tasksBySessionId.get(sessionID);
    if (taskId) {
      this.tasks.delete(taskId);
      this.tasksBySessionId.delete(sessionID);
    }
    unregisterSubagentSession(sessionID);
  }
}

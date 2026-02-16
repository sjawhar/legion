import type { PluginInput } from "@opencode-ai/plugin";
import {
  registerSubagentSession,
  unregisterSubagentSession,
} from "../hooks/subagent-question-blocker";
import { getAgentToolRestrictions } from "./agent-restrictions";
import { deleteTask, listTasks, writeTask } from "./task-storage";
import type { BackgroundTask, LaunchOptions } from "./types";

type OpencodeClient = PluginInput["client"];

function generateTaskId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `bg_${hex}`;
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
   * Rehydrate in-memory state from persisted task files.
   * Call once during plugin init to restore task visibility across restarts.
   */
  async rehydrate(opts?: { taskRetentionMs?: number }): Promise<void> {
    const tasks = await listTasks(this.directory);
    const now = Date.now();
    const ttl = opts?.taskRetentionMs;

    for (const task of tasks) {
      if (task.status === "pending" || task.status === "running") {
        task.status = "failed";
        task.error = "Interrupted: plugin restarted while task was in progress";
        task.completedAt = now;
        await writeTask(this.directory, task).catch((err) => {
          console.warn(`[rehydrate] Failed to persist failed status for ${task.id}:`, err);
        });
      }

      const taskAge = now - (task.completedAt ?? task.createdAt);
      if (ttl !== undefined && taskAge > ttl) {
        await deleteTask(this.directory, task.id).catch(() => {});
        continue;
      }

      this.tasks.set(task.id, task);
      // Don't index in tasksBySessionId — rehydrated tasks are all terminal
      // (completed/failed/cancelled) and shouldn't trigger todoContinuationEnforcer
    }
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
      parentSessionID: opts.parentSessionId,
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

      await writeTask(this.directory, task).catch((err) => {
        console.warn(`[background-manager] Failed to persist task ${task.id}:`, err);
      });

      this.startPrompt(task, opts).catch(() => {});
    } catch (err) {
      await this.finalize(task, "failed", {
        error: err instanceof Error ? err.message : String(err),
      });
      this.tasks.delete(task.id);
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
          tools: {
            ...getAgentToolRestrictions(opts.agent),
            question: false,
            askuserquestion: false,
          },
        },
        query: { directory: this.directory },
      });
    } catch (err) {
      await this.finalize(task, "failed", {
        error: err instanceof Error ? err.message : String(err),
      });
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
      const output = await this.fetchSessionOutput(task.sessionID);
      if (output) {
        task.result = output;
        return output;
      }
    }

    return "No output available";
  }

  async cancel(id: string): Promise<boolean> {
    const task = this.tasks.get(id);
    if (!task || (task.status !== "running" && task.status !== "pending")) {
      return false;
    }
    await this.finalize(task, "cancelled");
    if (task.sessionID) {
      this.client.session
        .abort({
          path: { id: task.sessionID },
          query: { directory: this.directory },
        })
        .catch(() => {});
    }
    return true;
  }

  async cancelAll(): Promise<number> {
    let count = 0;
    for (const task of this.tasks.values()) {
      if (
        (task.status === "running" || task.status === "pending") &&
        (await this.cancel(task.id))
      ) {
        count++;
      }
    }
    return count;
  }

  /**
   * Handle session.status events for completion detection.
   * Wire this into the plugin's event handler.
   */
  async handleSessionStatus(event: {
    type: string;
    properties?: Record<string, unknown>;
  }): Promise<void> {
    if (event.type !== "session.status") return;

    const sessionID = event.properties?.sessionID as string | undefined;
    if (!sessionID) return;

    const taskId = this.tasksBySessionId.get(sessionID);
    if (!taskId) return;

    const task = this.tasks.get(taskId);
    if (!task || task.status !== "running") return;

    const status = event.properties?.status as { type: string } | undefined;
    if (status?.type === "idle") {
      await this.finalize(task, "completed");
    } else if (status?.type === "error") {
      await this.finalize(task, "failed", { error: "Session entered error state" });
    }
  }

  /**
   * Clean up task data when a session is deleted.
   * Prevents memory leaks from accumulated completed tasks.
   */
  isBackgroundSession(sessionID: string): boolean {
    return this.tasksBySessionId.has(sessionID);
  }

  async cleanup(sessionID: string): Promise<void> {
    const taskId = this.tasksBySessionId.get(sessionID);
    if (!taskId) {
      unregisterSubagentSession(sessionID);
      return;
    }

    const task = this.tasks.get(taskId);
    if (task && (task.status === "pending" || task.status === "running")) {
      await this.finalize(task, "failed", { error: "Session deleted before completion" });
    }

    // Clean up runtime state. Task file persists on disk for TTL.
    // finalize() already cleaned tasksBySessionId and subagent session,
    // but we also remove from tasks Map to prevent memory accumulation.
    this.tasks.delete(taskId);
    this.tasksBySessionId.delete(sessionID);
    unregisterSubagentSession(sessionID);
  }

  async finalize(
    task: BackgroundTask,
    status: "completed" | "failed" | "cancelled",
    opts?: { error?: string }
  ): Promise<void> {
    if (task.status === "completed" || task.status === "failed" || task.status === "cancelled") {
      return;
    }

    task.status = status;
    task.completedAt = Date.now();
    if (opts?.error) {
      task.error = opts.error;
    }

    if (status === "completed" && task.sessionID) {
      const output = await this.fetchSessionOutput(task.sessionID);
      if (output) {
        task.result = output;
      }
    }

    if (task.sessionID) {
      this.tasksBySessionId.delete(task.sessionID);
      unregisterSubagentSession(task.sessionID);
    }

    try {
      await writeTask(this.directory, task);
    } catch (err) {
      console.warn(`[background-manager] Failed to persist task ${task.id}:`, err);
    }

    if (!task.sessionID) {
      this.tasks.delete(task.id);
    }
  }

  private async fetchSessionOutput(sessionID: string): Promise<string | null> {
    try {
      const messagesResult = await this.client.session.messages({
        path: { id: sessionID },
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
      return output.length > 0 ? output : null;
    } catch {
      return null;
    }
  }
}

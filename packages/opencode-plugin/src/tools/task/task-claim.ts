import { join } from "node:path";
import type { PluginInput, ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { acquireLock, getTaskDir, writeJsonAtomic } from "./storage";
import { readAllTasks } from "./task-list";
import { syncTaskTodoUpdate } from "./todo-sync";
import { LEASE_DURATION_MS, MAX_CLAIM_ATTEMPTS, SATISFYING_STATUSES, TaskSchema } from "./types";

export function createTaskClaimNextTool(ctx?: PluginInput, listId?: string): ToolDefinition {
  return tool({
    description:
      "Atomically claim the next ready+pending task.\n\n" +
      "Filters to pending tasks with all dependencies satisfied.\n" +
      "Excludes tasks with 3+ failed attempts (needs escalation).\n" +
      "Sets status=in_progress, owner, lease, and increments attempt_count.\n" +
      "Expired leases are auto-reclaimed.",
    args: {},
    execute: async (_args, context) => {
      const taskDir = getTaskDir(listId);
      const lock = acquireLock(taskDir);

      if (!lock.acquired) {
        return JSON.stringify({ error: "task_lock_unavailable" });
      }

      try {
        const allTasks = readAllTasks(taskDir);
        const taskMap = new Map(allTasks.map((t) => [t.id, t]));
        const now = Date.now();

        const reclaimExpired = () => {
          for (const task of allTasks) {
            if (task.status !== "in_progress") continue;
            const leaseExpires = task.metadata?.lease_expires_at;
            if (typeof leaseExpires === "number" && leaseExpires < now) {
              task.status = "pending";
              task.owner = undefined;
              if (task.metadata) {
                delete task.metadata.lease_expires_at;
                delete task.metadata.claimed_by_session;
              }
              writeJsonAtomic(join(taskDir, `${task.id}.json`), TaskSchema.parse(task));
            }
          }
        };

        reclaimExpired();

        const readyPending = allTasks
          .filter((task) => {
            if (task.status !== "pending") return false;

            const attemptCount =
              typeof task.metadata?.attempt_count === "number" ? task.metadata.attempt_count : 0;
            if (attemptCount >= MAX_CLAIM_ATTEMPTS) return false;

            const hasUnresolvedBlockers = task.blockedBy.some((blockerId) => {
              const blocker = taskMap.get(blockerId);
              return !blocker || !SATISFYING_STATUSES.has(blocker.status);
            });
            return !hasUnresolvedBlockers;
          })
          .sort((a, b) => a.id.localeCompare(b.id));

        if (readyPending.length === 0) {
          const escalated = allTasks.filter((t) => {
            const attempts =
              typeof t.metadata?.attempt_count === "number" ? t.metadata.attempt_count : 0;
            return t.status === "pending" && attempts >= MAX_CLAIM_ATTEMPTS;
          });

          return JSON.stringify({
            task: null,
            escalated: escalated.map((t) => ({
              id: t.id,
              subject: t.subject,
              attempt_count: t.metadata?.attempt_count,
            })),
          });
        }

        const target = readyPending[0];
        const prevAttempts =
          typeof target.metadata?.attempt_count === "number" ? target.metadata.attempt_count : 0;

        target.status = "in_progress";
        target.owner = context.sessionID;
        target.metadata = {
          ...target.metadata,
          lease_expires_at: now + LEASE_DURATION_MS,
          claimed_by_session: context.sessionID,
          attempt_count: prevAttempts + 1,
        };

        const validated = TaskSchema.parse(target);
        writeJsonAtomic(join(taskDir, `${validated.id}.json`), validated);

        await syncTaskTodoUpdate(ctx, validated, context.sessionID);

        return JSON.stringify({ task: validated });
      } finally {
        lock.release();
      }
    },
  });
}

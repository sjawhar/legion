import { join } from "node:path";
import type { PluginInput, ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { detectCycle } from "./graph";
import { acquireLock, getTaskDir, readJsonSafe, writeJsonAtomic } from "./storage";
import { readAllTasks } from "./task-list";
import { syncTaskTodoUpdate } from "./todo-sync";
import { TaskSchema, TaskUpdateInputSchema } from "./types";

const z = tool.schema;

const TASK_ID_PATTERN = /^T-[A-Za-z0-9-]+$/;

export function createTaskUpdateTool(ctx?: PluginInput, listId?: string): ToolDefinition {
  return tool({
    description:
      "Update an existing task.\n\n" +
      "Supports: subject, description, status, owner, metadata merge.\n" +
      "For deps use addBlocks/addBlockedBy (additive, not replacement).\n" +
      "Metadata: merge with existing, set key to null to delete.\n" +
      "Rejects with error if adding dependencies would create a cycle.",
    args: {
      id: z.string().describe("Task ID (required)"),
      subject: z.string().optional().describe("Task subject"),
      description: z.string().optional().describe("Task description"),
      status: z
        .enum(["pending", "in_progress", "completed", "cancelled"])
        .optional()
        .describe("Task status"),
      owner: z.string().optional().describe("Task owner"),
      addBlocks: z.array(z.string()).optional().describe("Task IDs to add to blocks (additive)"),
      addBlockedBy: z
        .array(z.string())
        .optional()
        .describe("Task IDs to add to blockedBy (additive)"),
      metadata: z
        .record(z.string(), z.unknown())
        .optional()
        .describe("Metadata to merge (null values delete keys)"),
      parentID: z.string().optional().describe("Parent task ID"),
    },
    execute: async (args, context) => {
      try {
        const validated = TaskUpdateInputSchema.parse(args);

        if (!TASK_ID_PATTERN.test(validated.id)) {
          return JSON.stringify({ error: "invalid_task_id" });
        }

        const taskDir = getTaskDir(listId);
        const lock = acquireLock(taskDir);

        if (!lock.acquired) {
          return JSON.stringify({ error: "task_lock_unavailable" });
        }

        let result: string;
        let validatedTask: ReturnType<typeof TaskSchema.parse> | null = null;
        try {
          const computeResult = () => {
            const taskPath = join(taskDir, `${validated.id}.json`);
            const task = readJsonSafe(taskPath, TaskSchema);

            if (!task) {
              return JSON.stringify({ error: "task_not_found" });
            }

            if (validated.subject !== undefined) task.subject = validated.subject;
            if (validated.description !== undefined) task.description = validated.description;
            if (validated.status !== undefined) task.status = validated.status;
            if (validated.status === "completed" || validated.status === "cancelled") {
              if (task.metadata) {
                delete task.metadata.lease_expires_at;
                delete task.metadata.claimed_by_session;
              }
            }
            if (validated.owner !== undefined) task.owner = validated.owner;
            if (validated.parentID !== undefined) task.parentID = validated.parentID;

            const addBlocks = validated.addBlocks;
            const addBlockedBy = validated.addBlockedBy;
            const warnings: string[] = [];

            const needsGraphCheck =
              (addBlocks && addBlocks.length > 0) || (addBlockedBy && addBlockedBy.length > 0);
            const allTasks = needsGraphCheck ? readAllTasks(taskDir) : [];
            const taskMap = needsGraphCheck ? new Map(allTasks.map((t) => [t.id, t])) : new Map();

            if (addBlockedBy && addBlockedBy.length > 0) {
              const newBlockedBy = [...new Set([...task.blockedBy, ...addBlockedBy])];

              taskMap.set(validated.id, { ...task, blockedBy: newBlockedBy });

              const cycle = detectCycle(validated.id, newBlockedBy, (id) => {
                const t = taskMap.get(id);
                return t ? { blocks: t.blocks, blockedBy: t.blockedBy } : null;
              });

              if (cycle) {
                return JSON.stringify({ error: "cycle_detected", cycle });
              }

              for (const depId of addBlockedBy) {
                if (!taskMap.has(depId)) {
                  warnings.push(`blockedBy references non-existent task ${depId}`);
                }
              }

              task.blockedBy = newBlockedBy;

              for (const depId of addBlockedBy) {
                const depPath = join(taskDir, `${depId}.json`);
                const depTask = readJsonSafe(depPath, TaskSchema);
                if (depTask) {
                  depTask.blocks = [...new Set([...depTask.blocks, validated.id])];
                  writeJsonAtomic(depPath, TaskSchema.parse(depTask));
                }
              }
            }

            if (addBlocks && addBlocks.length > 0) {
              const newBlocks = [...new Set([...task.blocks, ...addBlocks])];

              for (const blockedId of addBlocks) {
                const blockedTask = taskMap.get(blockedId);
                if (blockedTask) {
                  const proposedBlockedBy = [...new Set([...blockedTask.blockedBy, validated.id])];
                  const cycle = detectCycle(blockedId, proposedBlockedBy, (id) => {
                    if (id === blockedId)
                      return { blocks: blockedTask.blocks, blockedBy: proposedBlockedBy };
                    const t = taskMap.get(id);
                    return t ? { blocks: t.blocks, blockedBy: t.blockedBy } : null;
                  });

                  if (cycle) {
                    return JSON.stringify({ error: "cycle_detected", cycle });
                  }
                }
              }

              task.blocks = newBlocks;
            }

            if (validated.metadata !== undefined) {
              task.metadata = { ...task.metadata, ...validated.metadata };
              for (const key of Object.keys(task.metadata)) {
                if (task.metadata[key] === null) {
                  delete task.metadata[key];
                }
              }
            }

            validatedTask = TaskSchema.parse(task);
            writeJsonAtomic(taskPath, validatedTask);

            if (addBlocks && addBlocks.length > 0) {
              for (const blockedId of addBlocks) {
                const blockedPath = join(taskDir, `${blockedId}.json`);
                const blockedTask = readJsonSafe(blockedPath, TaskSchema);
                if (blockedTask) {
                  blockedTask.blockedBy = [...new Set([...blockedTask.blockedBy, validated.id])];
                  writeJsonAtomic(blockedPath, TaskSchema.parse(blockedTask));
                }
              }
            }

            const response: Record<string, unknown> = { task: validatedTask };
            if (warnings.length > 0) {
              response.warnings = warnings;
            }
            return JSON.stringify(response);
          };

          result = computeResult();
        } finally {
          lock.release();
        }
        if (validatedTask) {
          syncTaskTodoUpdate(ctx, validatedTask, context.sessionID).catch(() => {});
        }
        return result;
      } catch (error) {
        if (error instanceof Error && error.message.includes("Required")) {
          return JSON.stringify({ error: "validation_error", message: error.message });
        }
        return JSON.stringify({ error: "internal_error" });
      }
    },
  });
}

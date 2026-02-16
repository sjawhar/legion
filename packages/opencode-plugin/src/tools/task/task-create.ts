import { join } from "node:path";
import type { PluginInput, ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { detectCycle } from "./graph";
import { acquireLock, generateTaskId, getTaskDir, readJsonSafe, writeJsonAtomic } from "./storage";
import { indexPathFor, upsertIndexEntry } from "./task-index";
import { readAllTasks } from "./task-list";
import { syncTaskTodoUpdate } from "./todo-sync";
import { type Task, TaskCreateInputSchema, TaskSchema } from "./types";

const z = tool.schema;

export function createTaskCreateTool(ctx?: PluginInput, listId?: string): ToolDefinition {
  return tool({
    description:
      "Create a new task with auto-generated ID and threadID recording.\n\n" +
      "Auto-generates T-{uuid} ID, records threadID from context, sets status to pending.\n" +
      "Use blockedBy to declare dependencies for parallel execution planning.\n" +
      "Rejects with error if adding dependencies would create a cycle.",
    args: {
      subject: z.string().describe("Task subject in imperative form (e.g. 'Add error handling')"),
      description: z.string().optional().describe("Task description"),
      blocks: z.array(z.string()).optional().describe("Task IDs this task blocks"),
      blockedBy: z.array(z.string()).optional().describe("Task IDs blocking this task"),
      owner: z.string().optional().describe("Owner session/agent"),
      metadata: z.record(z.string(), z.unknown()).optional().describe("Task metadata"),
      parentID: z.string().optional().describe("Parent task ID for grouping"),
    },
    execute: async (args, context): Promise<string> => {
      try {
        const validated = TaskCreateInputSchema.parse(args);
        const taskDir = getTaskDir(listId);
        const lock = acquireLock(taskDir);

        if (!lock.acquired) {
          return JSON.stringify({ error: "task_lock_unavailable" });
        }

        let validatedTask: Task | null = null;
        let result: string;
        try {
          const computeResult = () => {
            const taskId = generateTaskId();

            const proposedBlockedBy = validated.blockedBy ?? [];
            const proposedBlocks = validated.blocks ?? [];

            const allTasks =
              proposedBlockedBy.length > 0 || proposedBlocks.length > 0
                ? readAllTasks(taskDir)
                : [];
            const taskMap = new Map(allTasks.map((t) => [t.id, t]));

            if (proposedBlockedBy.length > 0 || proposedBlocks.length > 0) {
              const getTaskWithVirtualEdges = (
                id: string
              ): { blocks: string[]; blockedBy: string[] } | null => {
                if (id === taskId) {
                  return { blocks: proposedBlocks, blockedBy: proposedBlockedBy };
                }
                const t = taskMap.get(id);
                if (!t) return null;
                const blockedBy = proposedBlocks.includes(id)
                  ? [...new Set([...t.blockedBy, taskId])]
                  : t.blockedBy;
                return { blocks: t.blocks, blockedBy };
              };

              const forwardCycle = detectCycle(taskId, proposedBlockedBy, getTaskWithVirtualEdges);
              if (forwardCycle) {
                return JSON.stringify({ error: "cycle_detected", cycle: forwardCycle });
              }

              for (const blockedId of proposedBlocks) {
                const virtualBlockedBy = [
                  ...new Set([...(taskMap.get(blockedId)?.blockedBy ?? []), taskId]),
                ];
                const blocksCycle = detectCycle(
                  blockedId,
                  virtualBlockedBy,
                  getTaskWithVirtualEdges
                );
                if (blocksCycle) {
                  return JSON.stringify({ error: "cycle_detected", cycle: blocksCycle });
                }
              }
            }

            const warnings: string[] = [];
            for (const depId of proposedBlockedBy) {
              if (!taskMap.has(depId) && allTasks.length > 0) {
                const depPath = join(taskDir, `${depId}.json`);
                const dep = readJsonSafe(depPath, TaskSchema);
                if (!dep) {
                  warnings.push(`blockedBy references non-existent task ${depId}`);
                }
              } else if (allTasks.length === 0) {
                const depPath = join(taskDir, `${depId}.json`);
                const dep = readJsonSafe(depPath, TaskSchema);
                if (!dep) {
                  warnings.push(`blockedBy references non-existent task ${depId}`);
                }
              }
            }

            const task: Task = {
              id: taskId,
              subject: validated.subject,
              description: validated.description ?? "",
              status: "pending",
              blocks: proposedBlocks,
              blockedBy: proposedBlockedBy,
              owner: validated.owner,
              metadata: validated.metadata,
              parentID: validated.parentID,
              threadID: context.sessionID,
            };

            validatedTask = TaskSchema.parse(task);
            writeJsonAtomic(join(taskDir, `${taskId}.json`), validatedTask);
            upsertIndexEntry(indexPathFor(taskDir), {
              id: validatedTask.id,
              status: validatedTask.status,
            });

            for (const blockedId of proposedBlocks) {
              const blockedPath = join(taskDir, `${blockedId}.json`);
              const blockedTask = readJsonSafe(blockedPath, TaskSchema);
              if (blockedTask) {
                blockedTask.blockedBy = [...new Set([...blockedTask.blockedBy, taskId])];
                writeJsonAtomic(blockedPath, TaskSchema.parse(blockedTask));
              }
            }

            const response: Record<string, unknown> = {
              task: { id: validatedTask.id, subject: validatedTask.subject },
            };
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
        if (error instanceof Error && error.name === "ZodError") {
          return JSON.stringify({ error: "validation_error", message: error.message });
        }
        return JSON.stringify({ error: "internal_error" });
      }
    },
  });
}

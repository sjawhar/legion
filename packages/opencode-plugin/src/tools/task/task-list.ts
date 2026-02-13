import { join } from "node:path";
import type { ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { getTaskDir, listTaskFiles, readJsonSafe } from "./storage";
import { SATISFYING_STATUSES, type Task, TaskSchema, type TaskStatus } from "./types";

const z = tool.schema;

export function readAllTasks(taskDir: string): Task[] {
  const fileIds = listTaskFiles(taskDir);
  const tasks: Task[] = [];
  for (const fileId of fileIds) {
    const task = readJsonSafe(join(taskDir, `${fileId}.json`), TaskSchema);
    if (task) {
      tasks.push(task);
    }
  }
  return tasks;
}

interface TaskSummary {
  id: string;
  subject: string;
  status: TaskStatus;
  owner?: string;
  blockedBy: string[];
  parentID?: string;
}

export function createTaskListTool(listId?: string): ToolDefinition {
  return tool({
    description:
      "List active tasks with summary information.\n\n" +
      "Excludes completed and cancelled by default.\n" +
      "Use ready=true to filter to tasks whose blockedBy are all completed/cancelled.",
    args: {
      ready: z.boolean().optional().describe("Filter to tasks with all dependencies satisfied"),
      parentID: z.string().optional().describe("Filter by parent task ID"),
    },
    execute: async (args) => {
      const typedArgs = args as { ready?: boolean; parentID?: string };
      const taskDir = getTaskDir(listId);
      const allTasks = readAllTasks(taskDir);

      let activeTasks = allTasks.filter(
        (task) => task.status !== "completed" && task.status !== "cancelled"
      );

      if (typedArgs.parentID) {
        activeTasks = activeTasks.filter((task) => task.parentID === typedArgs.parentID);
      }

      const taskMap = new Map(allTasks.map((t) => [t.id, t]));

      const summaries: TaskSummary[] = activeTasks.map((task) => {
        const unresolvedBlockers = task.blockedBy.filter((blockerId) => {
          const blocker = taskMap.get(blockerId);
          return !blocker || !SATISFYING_STATUSES.has(blocker.status);
        });

        return {
          id: task.id,
          subject: task.subject,
          status: task.status,
          owner: task.owner,
          blockedBy: unresolvedBlockers,
          parentID: task.parentID,
        };
      });

      const filtered = typedArgs.ready
        ? summaries.filter((s) => s.blockedBy.length === 0)
        : summaries;

      return JSON.stringify({ tasks: filtered });
    },
  });
}

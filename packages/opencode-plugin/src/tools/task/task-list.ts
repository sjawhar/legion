import { join } from "node:path";
import type { ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { getTaskDir, listTaskFiles, readJsonSafe } from "./storage";
import { indexPathFor, readTaskIndex } from "./task-index";
import { SATISFYING_STATUSES, type Task, TaskSchema, type TaskStatus } from "./types";

const z = tool.schema;

function readTasksFiltered(
  taskDir: string,
  filter: (task: Task) => boolean = () => true,
  useIndex: boolean = false
): Task[] {
  const index = useIndex ? readTaskIndex(indexPathFor(taskDir)) : null;
  const fileIds = index ? [...new Set(index.entries.map((e) => e.id))] : listTaskFiles(taskDir);
  const tasks: Task[] = [];
  for (const fileId of fileIds) {
    const task = readJsonSafe(join(taskDir, `${fileId}.json`), TaskSchema);
    if (task && filter(task)) {
      tasks.push(task);
    }
  }
  return tasks;
}

export function readActiveTasks(taskDir: string): Task[] {
  return readTasksFiltered(
    taskDir,
    (t) => t.status !== "completed" && t.status !== "cancelled",
    true
  );
}

/** Full disk scan — used by create/update for cycle detection (needs ALL tasks). */
export function readAllTasks(taskDir: string): Task[] {
  return readTasksFiltered(taskDir);
}

export function buildTaskMapWithBlockers(taskDir: string, activeTasks: Task[]): Map<string, Task> {
  const taskMap = new Map(activeTasks.map((t) => [t.id, t]));

  const missingBlockerIds = new Set<string>();
  for (const task of activeTasks) {
    for (const blockerId of task.blockedBy) {
      if (!taskMap.has(blockerId)) {
        missingBlockerIds.add(blockerId);
      }
    }
  }

  for (const blockerId of missingBlockerIds) {
    const blocker = readJsonSafe(join(taskDir, `${blockerId}.json`), TaskSchema);
    if (blocker) {
      taskMap.set(blocker.id, blocker);
    }
  }

  return taskMap;
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
    execute: async (args): Promise<string> => {
      const typedArgs = args as { ready?: boolean; parentID?: string };
      const taskDir = getTaskDir(listId);
      const activeTasks = readActiveTasks(taskDir);

      const filtered = typedArgs.parentID
        ? activeTasks.filter((task) => task.parentID === typedArgs.parentID)
        : activeTasks;

      const taskMap = buildTaskMapWithBlockers(taskDir, activeTasks);

      const summaries: TaskSummary[] = filtered.map((task) => {
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

      const result = typedArgs.ready
        ? summaries.filter((s) => s.blockedBy.length === 0)
        : summaries;

      return JSON.stringify({ tasks: result });
    },
  });
}

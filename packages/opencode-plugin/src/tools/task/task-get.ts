import { join } from "node:path";
import type { ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { getTaskDir, readJsonSafe } from "./storage";
import { TaskGetInputSchema, TaskSchema } from "./types";

const z = tool.schema;

const TASK_ID_PATTERN = /^T-[A-Za-z0-9-]+$/;

export function createTaskGetTool(listId?: string): ToolDefinition {
  return tool({
    description:
      "Retrieve a task by ID.\n\n" +
      "Returns the full task object including all fields.\n" +
      "Returns null if the task does not exist or the file is invalid.",
    args: {
      id: z.string().describe("Task ID to retrieve (format: T-{uuid})"),
    },
    execute: async (args) => {
      try {
        const validated = TaskGetInputSchema.parse(args);

        if (!TASK_ID_PATTERN.test(validated.id)) {
          return JSON.stringify({ error: "invalid_task_id" });
        }

        const taskDir = getTaskDir(listId);
        const taskPath = join(taskDir, `${validated.id}.json`);
        const task = readJsonSafe(taskPath, TaskSchema);

        return JSON.stringify({ task: task ?? null });
      } catch (error) {
        if (error instanceof Error && error.message.includes("validation")) {
          return JSON.stringify({ error: "invalid_arguments" });
        }
        return JSON.stringify({ error: "unknown_error" });
      }
    },
  });
}

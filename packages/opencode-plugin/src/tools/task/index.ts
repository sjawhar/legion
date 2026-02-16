import type { PluginInput, ToolDefinition } from "@opencode-ai/plugin";
import { createTaskClaimNextTool } from "./task-claim";
import { createTaskCreateTool } from "./task-create";
import { createTaskGetTool } from "./task-get";
import { createTaskListTool } from "./task-list";
import { createTaskUpdateTool } from "./task-update";

export { detectCycle } from "./graph";
export {
  indexPathFor,
  readTaskIndex,
  upsertIndexEntry,
  writeTaskIndexAtomic,
} from "./task-index";
export { readActiveTasks, readAllTasks } from "./task-list";
export type { TodoInfo } from "./todo-sync";
export { syncTaskTodoUpdate, syncTaskToTodo } from "./todo-sync";
export type {
  Task,
  TaskCreateInput,
  TaskIndex,
  TaskIndexEntry,
  TaskStatus,
  TaskUpdateInput,
} from "./types";

interface TaskTools {
  task_create: ToolDefinition;
  task_get: ToolDefinition;
  task_update: ToolDefinition;
  task_list: ToolDefinition;
  task_claim_next: ToolDefinition;
}

export function createTaskTools(ctx?: PluginInput, listId?: string): TaskTools {
  return {
    task_create: createTaskCreateTool(ctx, listId),
    task_get: createTaskGetTool(listId),
    task_update: createTaskUpdateTool(ctx, listId),
    task_list: createTaskListTool(listId),
    task_claim_next: createTaskClaimNextTool(ctx, listId),
  };
}

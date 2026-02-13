import type { PluginInput } from "@opencode-ai/plugin";
import { extractTodos as extractTodosRaw } from "../../hooks/utils";
import type { Task, TaskStatus } from "./types";

export interface TodoInfo {
  id: string;
  content: string;
  status: "pending" | "in_progress" | "completed" | "cancelled";
  priority?: "low" | "medium" | "high";
}

function mapTaskStatusToTodoStatus(taskStatus: TaskStatus): TodoInfo["status"] | null {
  switch (taskStatus) {
    case "pending":
      return "pending";
    case "in_progress":
      return "in_progress";
    case "completed":
      return "completed";
    case "cancelled":
      return null;
    default:
      return "pending";
  }
}

function extractPriority(metadata?: Record<string, unknown>): TodoInfo["priority"] | undefined {
  if (!metadata) return undefined;
  const priority = metadata.priority;
  if (typeof priority === "string" && ["low", "medium", "high"].includes(priority)) {
    return priority as "low" | "medium" | "high";
  }
  return undefined;
}

export function syncTaskToTodo(task: Task): TodoInfo | null {
  const todoStatus = mapTaskStatusToTodoStatus(task.status);
  if (todoStatus === null) {
    return null;
  }
  return {
    id: task.id,
    content: task.subject,
    status: todoStatus,
    priority: extractPriority(task.metadata),
  };
}

function extractTodos(response: unknown): TodoInfo[] {
  return extractTodosRaw(response) as TodoInfo[];
}

export async function syncTaskTodoUpdate(
  ctx: PluginInput | undefined,
  task: Task,
  sessionID: string
): Promise<void> {
  if (!ctx) return;

  try {
    const sessionApi = ctx.client.session as {
      todo: (input: { path: { id: string } }) => Promise<unknown>;
      todoUpdate?: (input: {
        path: { id: string };
        body: { todos: TodoInfo[] };
      }) => Promise<unknown>;
    };
    const response = await sessionApi.todo({
      path: { id: sessionID },
    });
    const currentTodos = extractTodos(response);
    const nextTodos = currentTodos.filter((todo) => todo.id !== task.id);
    const todo = syncTaskToTodo(task);

    if (todo) {
      nextTodos.push(todo);
    }

    if (sessionApi.todoUpdate) {
      await sessionApi.todoUpdate({
        path: { id: sessionID },
        body: { todos: nextTodos },
      });
    }
  } catch (err) {
    // Todo sync is best-effort, but log first failure for observability
    console.warn(`[task] todo sync failed: ${err instanceof Error ? err.message : String(err)}`);
  }
}

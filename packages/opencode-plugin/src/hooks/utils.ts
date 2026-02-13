/**
 * Shared utilities for hook modules.
 *
 * `extractTodos` and `resolveSessionID` were duplicated across
 * todo-continuation-enforcer, compaction-todo-preserver, and
 * tools/task/todo-sync. Centralised here to prevent drift.
 */

export interface TodoItem {
  id: string;
  content: string;
  status: string;
  priority?: string;
}

/**
 * Extract todo items from an SDK response.
 * Handles both `{ data: TodoItem[] }` wrapper and raw `TodoItem[]`.
 */
export function extractTodos(response: unknown): TodoItem[] {
  const payload = response as { data?: unknown };
  if (Array.isArray(payload?.data)) return payload.data as TodoItem[];
  if (Array.isArray(response)) return response as TodoItem[];
  return [];
}

/**
 * Resolve a session ID from event properties.
 * Handles both `{ sessionID }` and `{ info: { id } }` shapes.
 */
export function resolveSessionID(props?: Record<string, unknown>): string | undefined {
  return (props?.sessionID ?? (props?.info as { id?: string } | undefined)?.id) as
    | string
    | undefined;
}

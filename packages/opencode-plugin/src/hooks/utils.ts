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

export function isRecord(value: unknown): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value);
}

/**
 * Extract todo items from an SDK response.
 * Handles both `{ data: TodoItem[] }` wrapper and raw `TodoItem[]`.
 */
export function extractTodos(response: unknown): TodoItem[] {
  const payload = isRecord(response) ? response : undefined;
  if (Array.isArray(payload?.data)) return payload.data as TodoItem[];
  if (Array.isArray(response)) return response as TodoItem[];
  return [];
}

/**
 * Resolve a session ID from event properties.
 * Handles both `{ sessionID }` and `{ info: { id } }` shapes.
 */
export function resolveSessionID(props?: Record<string, unknown>): string | undefined {
  const sessionID = props?.sessionID;
  if (typeof sessionID === "string") return sessionID;
  const info = props?.info;
  if (isRecord(info) && typeof info.id === "string") return info.id;
  return undefined;
}

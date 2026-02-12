import type { PluginInput } from "@opencode-ai/plugin";
import { extractTodos, resolveSessionID, type TodoItem } from "./utils";

const SNAPSHOT_TTL_MS = 5 * 60 * 1000;

type TodoSnapshot = TodoItem;

interface TimestampedSnapshot {
  todos: TodoSnapshot[];
  capturedAt: number;
}

export interface CompactionTodoPreserver {
  capture: (sessionID: string) => Promise<void>;
  event: (input: { event: { type: string; properties?: unknown } }) => Promise<void>;
}

export function createCompactionTodoPreserverHook(ctx: PluginInput): CompactionTodoPreserver {
  const snapshots = new Map<string, TimestampedSnapshot>();

  const pruneStale = (): void => {
    const now = Date.now();
    for (const [id, snap] of snapshots) {
      if (now - snap.capturedAt > SNAPSHOT_TTL_MS) {
        snapshots.delete(id);
      }
    }
  };

  const capture = async (sessionID: string): Promise<void> => {
    if (!sessionID) return;
    pruneStale();
    try {
      const response = await ctx.client.session.todo({ path: { id: sessionID } });
      const todos = extractTodos(response);
      if (todos.length === 0) {
        snapshots.delete(sessionID);
        return;
      }
      snapshots.set(sessionID, { todos, capturedAt: Date.now() });
    } catch (err) {
      console.warn("[opencode-legion] Failed to capture todos for compaction:", err);
    }
  };

  const mergeTodos = (
    snapshot: TodoSnapshot[],
    current: TodoSnapshot[]
  ): { merged: TodoSnapshot[]; changed: boolean } => {
    const currentById = new Map(current.map((t) => [t.id, t]));
    const merged = [...current];
    let changed = false;
    for (const snapped of snapshot) {
      if (!currentById.has(snapped.id)) {
        merged.push(snapped);
        changed = true;
      }
    }
    return { merged, changed };
  };

  const restore = async (sessionID: string): Promise<void> => {
    const entry = snapshots.get(sessionID);
    if (!entry || entry.todos.length === 0) return;

    let currentTodos: TodoSnapshot[] = [];
    try {
      const response = await ctx.client.session.todo({ path: { id: sessionID } });
      currentTodos = extractTodos(response) as TodoSnapshot[];
    } catch (err) {
      console.warn("[opencode-legion] Failed to check current todos during restore:", err);
    }

    const { merged, changed } = mergeTodos(entry.todos, currentTodos);
    if (!changed) {
      snapshots.delete(sessionID);
      return;
    }

    const sessionApi = ctx.client.session as unknown as Record<string, CallableFunction>;
    if (!sessionApi.todoUpdate) {
      snapshots.delete(sessionID);
      return;
    }

    try {
      await sessionApi.todoUpdate({
        path: { id: sessionID },
        body: { todos: merged },
      });
    } catch (err) {
      console.warn("[opencode-legion] Failed to restore todos after compaction:", err);
    } finally {
      snapshots.delete(sessionID);
    }
  };

  const event = async ({
    event,
  }: {
    event: { type: string; properties?: unknown };
  }): Promise<void> => {
    const props = event.properties as Record<string, unknown> | undefined;

    if (event.type === "session.deleted") {
      const sessionID = resolveSessionID(props);
      if (sessionID) {
        snapshots.delete(sessionID);
      }
      return;
    }

    if (event.type === "session.compacted") {
      const sessionID = resolveSessionID(props);
      if (sessionID) {
        await restore(sessionID);
      }
    }
  };

  return { capture, event };
}

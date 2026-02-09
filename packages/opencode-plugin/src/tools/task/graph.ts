interface TaskNode {
  blocks: string[];
  blockedBy: string[];
}

type TaskLookup = (id: string) => TaskNode | null;

export function detectCycle(
  taskId: string,
  proposedBlockedBy: string[],
  getTask: TaskLookup
): string[] | null {
  for (const depId of proposedBlockedBy) {
    const path = canReach(depId, taskId, getTask, new Set());
    if (path) {
      return [taskId, ...path];
    }
  }
  return null;
}

function canReach(
  from: string,
  target: string,
  getTask: TaskLookup,
  visited: Set<string>
): string[] | null {
  if (from === target) return [from];
  if (visited.has(from)) return null;
  visited.add(from);

  const node = getTask(from);
  if (!node) return null;

  for (const upstream of node.blockedBy) {
    const path = canReach(upstream, target, getTask, visited);
    if (path) return [from, ...path];
  }

  return null;
}

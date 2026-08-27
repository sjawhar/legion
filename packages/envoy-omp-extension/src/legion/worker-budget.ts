import type { LegionRole } from "@legion/contracts";

export type WorkerSession = {
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly token: string;
  readonly spawnToken: string;
  readonly secret: string;
  readonly agentId: string;
};

export type WorkerSpawn = {
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly token: string;
  readonly spawnToken: string;
  readonly workspace: string;
};

export type PendingLegionSpawn = {
  readonly toolCallId: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly token: string;
  readonly spawnToken: string;
  readonly release: () => void;
};

export type RootBootstrap = { readonly role: string; readonly secret: string };

export type WorkerRuntimeState = {
  readonly budgetPermits: Map<string, () => void>;
  readonly budgetAcquisitions: Map<string, Promise<void>>;
  readonly pendingSpawns: Map<string, Set<PendingLegionSpawn>>;
  readonly pendingSpawnsByToken: Map<string, PendingLegionSpawn>;
  readonly rootBootstraps: Map<string, Promise<RootBootstrap>>;
  readonly sessions: Map<string, WorkerSession>;
  readonly budgetWaiters: (() => void)[];
  budgetLimit: number | undefined;
  budgetInUse: number;
};

// Task sessions load this extension through distinct module URLs, but their worker
// reservations and role backing form one lifecycle within the OMP process.
const workerRuntimeStateKey = Symbol.for("legion.envoy-omp-extension.worker-runtime-state");
const sharedWorkerRuntime = globalThis as typeof globalThis & {
  [key: symbol]: WorkerRuntimeState | undefined;
};
const existingWorkerRuntime = sharedWorkerRuntime[workerRuntimeStateKey];
const workerRuntime: WorkerRuntimeState = existingWorkerRuntime ?? {
  budgetPermits: new Map<string, () => void>(),
  budgetAcquisitions: new Map<string, Promise<void>>(),
  pendingSpawns: new Map<string, Set<PendingLegionSpawn>>(),
  pendingSpawnsByToken: new Map<string, PendingLegionSpawn>(),
  rootBootstraps: new Map<string, Promise<RootBootstrap>>(),
  sessions: new Map<string, WorkerSession>(),
  budgetWaiters: [],
  budgetLimit: undefined,
  budgetInUse: 0,
};
if (!existingWorkerRuntime) sharedWorkerRuntime[workerRuntimeStateKey] = workerRuntime;

const workerBudgetPermits = workerRuntime.budgetPermits;
const workerBudgetAcquisitions = workerRuntime.budgetAcquisitions;
export const pendingLegionSpawns = workerRuntime.pendingSpawns;
export const pendingLegionSpawnsByToken = workerRuntime.pendingSpawnsByToken;
export const rootBootstraps = workerRuntime.rootBootstraps;
export const workerSessions = workerRuntime.sessions;
const workerBudgetWaiters = workerRuntime.budgetWaiters;

export function registerWorkerBudgetPermit(sessionID: string, release: () => void): void {
  if (workerBudgetPermits.has(sessionID)) {
    throw new Error(`Legion worker budget permit already registered for ${sessionID}`);
  }
  workerBudgetPermits.set(sessionID, release);
}

export function releaseWorkerBudgetPermit(sessionID: string): void {
  const release = workerBudgetPermits.get(sessionID);
  if (!release) return;
  workerBudgetPermits.delete(sessionID);
  release();
}

export async function acquireWorkerBudget(limit: number): Promise<() => void> {
  if (workerRuntime.budgetLimit === undefined) workerRuntime.budgetLimit = limit;
  if (
    workerRuntime.budgetLimit !== limit &&
    workerRuntime.budgetInUse === 0 &&
    workerBudgetWaiters.length === 0
  ) {
    workerRuntime.budgetLimit = limit;
  }
  if (workerRuntime.budgetInUse >= workerRuntime.budgetLimit) {
    await new Promise<void>((resolve) => workerBudgetWaiters.push(resolve));
  } else {
    workerRuntime.budgetInUse += 1;
  }
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const waiter = workerBudgetWaiters.shift();
    if (waiter) {
      waiter();
      return;
    }
    workerRuntime.budgetInUse -= 1;
  };
}

export async function ensureWorkerBudgetPermit(sessionID: string): Promise<void> {
  if (workerBudgetPermits.has(sessionID)) return;
  const pending = workerBudgetAcquisitions.get(sessionID);
  if (pending) return await pending;
  const acquisition = (async () => {
    if (workerBudgetPermits.has(sessionID)) return;
    const release = await acquireWorkerBudget(Number(process.env.LEGION_WORKER_BUDGET ?? "6"));
    try {
      if (workerBudgetPermits.has(sessionID)) {
        release();
        return;
      }
      registerWorkerBudgetPermit(sessionID, release);
    } catch (error) {
      release();
      throw error;
    }
  })();
  workerBudgetAcquisitions.set(sessionID, acquisition);
  try {
    await acquisition;
  } finally {
    if (workerBudgetAcquisitions.get(sessionID) === acquisition)
      workerBudgetAcquisitions.delete(sessionID);
  }
}

export function addPendingLegionSpawn(pending: PendingLegionSpawn): void {
  const forToolCall = pendingLegionSpawns.get(pending.toolCallId) ?? new Set<PendingLegionSpawn>();
  forToolCall.add(pending);
  pendingLegionSpawns.set(pending.toolCallId, forToolCall);
  pendingLegionSpawnsByToken.set(pending.spawnToken, pending);
}

function removePendingLegionSpawn(pending: PendingLegionSpawn): void {
  const forToolCall = pendingLegionSpawns.get(pending.toolCallId);
  if (forToolCall) {
    forToolCall.delete(pending);
    if (forToolCall.size === 0) pendingLegionSpawns.delete(pending.toolCallId);
  }
  pendingLegionSpawnsByToken.delete(pending.spawnToken);
}

export function releasePendingLegionSpawn(pending: PendingLegionSpawn): void {
  removePendingLegionSpawn(pending);
  pending.release();
}

export function transferPendingLegionSpawn(pending: PendingLegionSpawn): () => void {
  removePendingLegionSpawn(pending);
  return pending.release;
}

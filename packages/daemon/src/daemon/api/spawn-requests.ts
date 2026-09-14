import type { SpawnWorkerResponse } from "@legion/contracts";
import type { LegionState, PersistedSpawnRequest } from "../legion-state";
import { HttpError } from "./http";

/** How long a fulfilled spawn request's result is kept for a repeat of its `requestId` to be
 * answered from — comfortably longer than the plugin's whole retry budget
 * (`SPAWN_WORKER_RETRY_DELAYS_MS` in pi-envoy's daemon-client) plus a daemon that is slow to
 * answer, short enough that the durable ledger only holds one tree's recent spawns. */
export const SPAWN_REQUEST_RETENTION_MS = 10 * 60_000;

/** The body a spawn request id belongs to. A repeat naming a different tree, issue, role, or
 * task is refused (409): an id belongs to one request. */
export type SpawnRequestKey = Pick<PersistedSpawnRequest, "tree" | "issue" | "role" | "task">;

interface SpawnRequestEntry {
  key: SpawnRequestKey;
  result: Promise<SpawnWorkerResponse>;
}

/**
 * Dedupes `/legion/v1/worker/spawn` by the plugin-minted `requestId` (LEGION-102): a spawn whose
 * HTTP response was lost — the plugin retries a rejected fetch with the same id — must never queue
 * the task twice. An in-memory promise shares an in-flight request; before that promise resolves,
 * its fulfilled result is written to `LegionState` in the same save that makes the handler
 * successful. A daemon restarted after that save reads the retained result and returns it without
 * touching ProcessManager. Rejected attempts are not recorded; the same id may be tried again.
 * No timer — each request sweeps expired durable entries against the injected clock.
 */
export class SpawnRequestLedger {
  private readonly entries = new Map<string, SpawnRequestEntry>();

  constructor(
    private readonly state: LegionState,
    private readonly now: () => number,
    private readonly save: () => Promise<void>
  ) {}

  settle(
    requestId: string,
    key: SpawnRequestKey,
    run: () => Promise<SpawnWorkerResponse>
  ): Promise<SpawnWorkerResponse> {
    this.sweep();
    const existing = this.entries.get(requestId) ?? this.persistedEntry(requestId);
    if (existing) {
      if (
        existing.key.tree !== key.tree ||
        existing.key.issue !== key.issue ||
        existing.key.role !== key.role ||
        existing.key.task !== key.task
      ) {
        throw new HttpError(
          409,
          `Spawn request ${requestId} already names a different tree, issue, role, or task; a request id belongs to one request`
        );
      }
      console.info(
        `[legion] spawn request ${requestId} repeated for ${key.role} on ${key.issue}; answering the original result`
      );
      return existing.result;
    }

    const result = run().then(async (response) => {
      this.state.spawnRequests[requestId] = { ...key, result: response, settledAt: this.now() };
      await this.save();
      return response;
    });
    const entry: SpawnRequestEntry = { key, result };
    this.entries.set(requestId, entry);
    void result.then(
      () => {
        if (this.entries.get(requestId) === entry) this.entries.delete(requestId);
      },
      () => {
        if (this.entries.get(requestId) === entry) this.entries.delete(requestId);
      }
    );
    return result;
  }

  private persistedEntry(requestId: string): SpawnRequestEntry | undefined {
    const request = this.state.spawnRequests[requestId];
    return request === undefined
      ? undefined
      : { key: request, result: Promise.resolve(request.result) };
  }

  private sweep(): void {
    const cutoff = this.now() - SPAWN_REQUEST_RETENTION_MS;
    for (const [requestId, entry] of Object.entries(this.state.spawnRequests)) {
      if (entry.settledAt <= cutoff) delete this.state.spawnRequests[requestId];
    }
  }
}

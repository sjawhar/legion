import type { IssueKey } from "@legion/contracts";

/** Thrown by any `ProcessManager` operation that touches a tree's admission, claims, or panes
 * while that tree is closing (`closingTrees` names it) or already closed/absent. A route handler
 * translates this into an HTTP 409. Shared between `processes.ts` and `worker-admission.ts` (a
 * dedicated module, not a re-export from either, to avoid a circular import between them). */
export class TreeClosingError extends Error {
  constructor(readonly treeKey: IssueKey) {
    super(`Legion tree ${treeKey} is closing`);
    this.name = "TreeClosingError";
  }
}

/** Thrown when `stopProcess` cannot confirm a process actually stopped — a real `kill-pane`
 * failure, not the expected "can't find pane" race against a process that already exited on its
 * own. A route handler translates this into an HTTP 502: the caller must never treat a claim as
 * safely replaceable, or a tree as safely closed, while the process it names might still be
 * alive and running. */
export class StopFailed extends Error {
  constructor(
    readonly token: string,
    message: string
  ) {
    super(message);
    this.name = "StopFailed";
  }
}

/** Why an acknowledged prompt's turn was not observed to start. `"no-turn"`: the socket is still
 * up and the worker simply started nothing within the bound — a swallowed prompt, which every
 * prompt site counts and retries exactly like a refusal. `"socket-closed"`: the shim's socket
 * closed before any turn began — a dead worker (or shim), not a slow one; nothing about the
 * prompt is counted, and the socket-close handler (`onWorkerClientClosed`) owns what happens to
 * the claim, so no prompt site touches it twice. */
export type PromptNotStartedReason = "no-turn" | "socket-closed";

/** Thrown by `ProcessManager.promptExistingWorker` when a live worker's shim acknowledged a
 * prompt but no turn was observed to start (`agent_start`, or `get_state` reporting a stream)
 * within `boundMs`. The acknowledgement alone is not delivery: OMP answers it before the turn
 * begins and can accept a message that starts none. `reason` says which of the two outcomes this
 * is (see `PromptNotStartedReason`); it lives in this shared module for the same reason
 * `TreeClosingError`/`StopFailed` do (both `processes.ts` and `worker-admission.ts` import it).
 * `observation` names what was seen: `get_state: isStreaming=false`, `get_state failed:
 * <message>`, or `socket closed` (optionally with the `get_state` failure the close caused). */
export class PromptNotStarted extends Error {
  constructor(
    readonly token: string,
    readonly boundMs: number,
    readonly reason: PromptNotStartedReason,
    observation: string
  ) {
    super(
      `worker ${token} acknowledged the prompt but started no turn within ${boundMs}ms (${observation})`
    );
    this.name = "PromptNotStarted";
  }
}

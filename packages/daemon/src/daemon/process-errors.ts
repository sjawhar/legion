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

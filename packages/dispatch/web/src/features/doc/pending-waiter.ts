import type { PendingState } from "./pending-sync";

interface Wait {
  readonly artifactId: string;
  answer(): void;
}

/**
 * Who waits for this browser's edits of a document to reach the server, beside the pending report
 * that answers them (`pending-sync.ts`, through `ProofDocument`'s `onPending`).
 *
 * The margin waits here after it has written the mark a refused send is to anchor to again
 * (`reanchor`), so the mark its retry names is one the server can already see. That mark is an
 * update of its own, so a report follows it - the store counts a local update only once its
 * IndexedDB write is through - and only a report taken after the wait begins can be one that
 * counts it. The last report before it was taken without it, and a document with nothing else
 * pending reports nothing pending there: answering from it would send the retry straight back
 * into the refusal it is recovering from. A count another edit's acknowledgement asked for
 * before the re-mark's own write was issued is no report at all - `pending-sync.ts` drops it
 * rather than give a count that reads a document without that row - so every report that
 * reaches here counts every row this tab has written.
 */
export interface PendingWaiter {
  /** Takes the pending report `state` of the document `artifactId`. */
  report(artifactId: string, state: PendingState): void;
  /**
   * Answers once a report of `artifactId` taken after this call says this browser holds no edit
   * of it the server has still to take, and at `timeoutMs` whatever the document has reported:
   * the bound is the backstop for a document that reports nothing at all - a socket in reconnect
   * backoff, a row no admission here can send - and the caller posts anyway, as the margin does.
   * A report of another document never answers it: the page can move to one while the wait is
   * out, and that document's rows say nothing about this one's mark.
   */
  whenSent(artifactId: string, timeoutMs: number): Promise<void>;
}

export function createPendingWaiter(): PendingWaiter {
  const waiting = new Set<Wait>();
  return {
    report(artifactId, state) {
      // The count is every tab's, and a read-only admission cannot send the rows it holds:
      // neither is this page's to make go, so a wait on one of them ends at its bound.
      if (state.count !== 0 || state.readOnly) return;
      // A wait only ever drops itself, which Set iteration already allows, and none adds one: no
      // copy of the set is needed on a callback every edit fires.
      for (const wait of waiting) {
        if (wait.artifactId === artifactId) wait.answer();
      }
    },
    whenSent: (artifactId, timeoutMs) =>
      new Promise<void>((resolve) => {
        const wait: Wait = {
          artifactId,
          answer: () => {
            waiting.delete(wait);
            clearTimeout(bound);
            resolve();
          },
        };
        const bound = setTimeout(() => {
          waiting.delete(wait);
          resolve();
        }, timeoutMs);
        waiting.add(wait);
      }),
  };
}

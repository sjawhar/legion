import { useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import type { UserIssueState, UserState } from "../../api/types";
import { dangerText } from "../../theme/classes";
import { stateForIssue } from "./pins";
import {
  applyPinStateOperation,
  issueStateTransport,
  type PinStateOperation,
  sharedIssueStateWrites,
} from "./state-write-queue";

interface FailedStateOperations {
  authoritativeState: UserIssueState | undefined;
  issueKey: string;
  operations: PinStateOperation[];
}

export interface PinWrites {
  /** The refusal banner, rendered wherever the surface puts it; null while nothing is refused. */
  banner: ReactNode;
  /** A refusal is waiting for the reader: every pin control the surface draws is disabled, and
   *  `updatePins` takes nothing, until Retry or Close answers it. */
  blocked: boolean;
  /** Pins or unpins one item, optimistically, and queues the write behind it. The operation may
   *  be derived from the stored `dismissed` list at the moment of the write, which is what a
   *  toggle needs: the list it reads is the one the write will be applied to. */
  updatePins: (operation: PinStateOperation | ((dismissed: string[]) => PinStateOperation)) => void;
}

/**
 * The one pin-write surface every tab of an issue shares: the optimistic write, the queued one
 * behind it, and - when the server refuses it - a banner saying so with Retry and Close, which
 * holds further pin writes until the reader answers it. A silent revert reads as a broken Pin
 * button, so a refusal is always said out loud, whichever surface made the write.
 *
 * The Conversation and the Activity tab both draw `Pin`, and each holds its own failure: the
 * banner belongs to the surface whose write was refused, its controls are the ones that stop
 * taking writes, and its Retry replays exactly the operations the queue handed back. The write
 * itself is the same one either way - `sharedIssueStateWrites` keeps every surface's pins in
 * one order - so a Retry here never races a pin made on the other tab.
 */
export function usePinWrites(issueKey: string): PinWrites {
  const queryClient = useQueryClient();
  const [failedOps, setFailedOps] = useState<FailedStateOperations>();
  const [retrying, setRetrying] = useState(false);
  const blocked = failedOps?.issueKey === issueKey;

  const enqueuePinOperations = (operations: PinStateOperation[]) => {
    for (const operation of operations) {
      void sharedIssueStateWrites
        .enqueue(issueKey, operation, {
          ...issueStateTransport,
          onDrained: (key, next) => {
            queryClient.setQueryData<UserState>(["user-state"], (current) => ({
              ...current,
              [key]: next,
            }));
            setFailedOps((current) => (current?.issueKey === key ? undefined : current));
            setRetrying(false);
          },
          onError: (key, failedOperations, next) => {
            if (next === undefined) {
              void queryClient.invalidateQueries({ queryKey: ["user-state"] });
            } else {
              queryClient.setQueryData<UserState>(["user-state"], (current) => ({
                ...current,
                [key]: next,
              }));
            }
            setFailedOps({
              authoritativeState: next,
              issueKey: key,
              operations: failedOperations,
            });
            setRetrying(false);
          },
        })
        .catch(() => {});
    }
  };

  const updatePins = (
    operation: PinStateOperation | ((dismissed: string[]) => PinStateOperation)
  ) => {
    if (blocked) {
      return;
    }
    let nextOperation: PinStateOperation | undefined;
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      const currentIssueState = stateForIssue(current, issueKey);
      nextOperation =
        typeof operation === "function" ? operation(currentIssueState.dismissed) : operation;
      return {
        ...current,
        [issueKey]: {
          ...currentIssueState,
          dismissed: applyPinStateOperation(currentIssueState.dismissed, nextOperation),
        },
      };
    });
    if (nextOperation !== undefined) {
      enqueuePinOperations([nextOperation]);
    }
  };

  const retryFailedOperations = () => {
    if (failedOps === undefined || failedOps.issueKey !== issueKey || retrying) {
      return;
    }
    const operations = failedOps.operations;
    setRetrying(true);
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      let next = stateForIssue(current, issueKey);
      for (const operation of operations) {
        next = { ...next, dismissed: applyPinStateOperation(next.dismissed, operation) };
      }
      return { ...current, [issueKey]: next };
    });
    enqueuePinOperations(operations);
  };

  const dismissFailedOperations = () => {
    if (failedOps === undefined || failedOps.issueKey !== issueKey || retrying) {
      return;
    }
    const authoritativeState = failedOps.authoritativeState;
    if (authoritativeState === undefined) {
      void queryClient.invalidateQueries({ queryKey: ["user-state"] });
    } else {
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [issueKey]: authoritativeState,
      }));
    }
    setFailedOps(undefined);
  };

  return {
    banner: blocked ? (
      <div className={`flex items-center gap-3 text-sm ${dangerText}`} role="alert">
        <p>Couldn't save pin — retry</p>
        <button
          className="font-medium underline disabled:cursor-not-allowed disabled:opacity-50"
          disabled={retrying}
          onClick={retryFailedOperations}
          type="button"
        >
          Retry
        </button>
        <button
          className="underline disabled:cursor-not-allowed disabled:opacity-50"
          disabled={retrying}
          onClick={dismissFailedOperations}
          type="button"
        >
          Close
        </button>
      </div>
    ) : null,
    blocked,
    updatePins,
  };
}

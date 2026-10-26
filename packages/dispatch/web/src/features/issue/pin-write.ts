import type { QueryClient } from "@tanstack/react-query";

import { ApiError, api, type WriteBounds } from "../../api/client";
import type { UserIssueState, UserState } from "../../api/types";
import { type Shape, shape } from "../writes/write-storage";
import { defineWriteKind, ofKind, type Writes, writesOf } from "../writes/write-store";
import { pinnedItemMarker, stateForIssue } from "./pins";

export type PinStateOperation = { readonly id: string; readonly op: "pin" | "unpin" };

/** One pin or unpin as the write store holds it: the issue's, and the item. */
interface PinWrite {
  readonly issueKey: string;
  readonly operation: PinStateOperation;
}

const keptPin = shape.record({
  issueKey: shape.string,
  operation: shape.record({ id: shape.string, op: shape.oneOf("pin", "unpin") }),
}) satisfies Shape<PinWrite>;

/** `dismissed` with `operation` applied: the item's marker in it for a pin, out of it for an
 *  unpin. */
export function applyPinStateOperation(
  dismissed: readonly string[],
  operation: PinStateOperation
): string[] {
  const marker = pinnedItemMarker(operation.id);
  if (operation.op === "unpin") return dismissed.filter((item) => item !== marker);
  return dismissed.includes(marker) ? [...dismissed] : [...dismissed, marker];
}

/** The viewer's state of each issue as Dispatch last answered it, which each pin writes over:
 *  every pin of one issue goes after the one before it (`pin:<issue>`), so each reads the answer
 *  its predecessor got. */
const authoritative = new Map<string, UserIssueState>();

/** Dispatch's current row, when it refused a write for carrying an older sequence. */
function staleState(error: unknown): UserIssueState | undefined {
  return error instanceof ApiError && error.code === "STATE_STALE" ? error.state : undefined;
}

/**
 * One attempt of a pin: the viewer's state of the issue - as the last pin's answer left it, or
 * read afresh - with the item pinned or unpinned, put back under the next sequence. Dispatch
 * answers a write another tab or device got in first with `STATE_STALE` and its current row,
 * which the pin is put on once more.
 */
function pinAttempt({ issueKey, operation }: PinWrite) {
  return async (bounds: WriteBounds): Promise<UserIssueState> => {
    let base = authoritative.get(issueKey) ?? stateForIssue(await api.getMyState(), issueKey);
    for (let stale = 0; ; stale += 1) {
      try {
        const next = await api.putIssueState(
          issueKey,
          { dismissed: applyPinStateOperation(base.dismissed, operation), seq: base.seq + 1 },
          bounds
        );
        authoritative.set(issueKey, next);
        return next;
      } catch (error) {
        const current = staleState(error);
        if (current === undefined || stale > 0) throw error;
        authoritative.set(issueKey, current);
        base = current;
      }
    }
  };
}

/** Whether `writes` still holds a pin of `issueKey` out: the page's state of it is still the
 *  optimistic one. */
function pinsOut(writes: Writes, issueKey: string): boolean {
  return writes.rows().some((row) => {
    const pin = ofKind(row, pinWrite);
    return (
      pin?.detail.issueKey === issueKey &&
      (pin.status === "queued" || pin.status === "sending" || pin.status === "late")
    );
  });
}

/**
 * A pin or an unpin, one write of the store each, ordered on its issue (`pin:<issue>`), shown in
 * the shell's indicator and kept across a reload. The surface applies it to `["user-state"]` at
 * once; once the issue's last pin lands, the state Dispatch answered replaces what the page
 * showed and the next pin reads the state afresh, and a refused, discarded or dropped one has the
 * page read its state again. The states pins' answers left go with the person signed in
 * (`reset`), so another person's next pin never writes over them.
 */
const pinWrite = defineWriteKind<PinWrite>({
  kept: keptPin,
  landed: (client, write, answer) => {
    const { issueKey } = write.detail;
    if (pinsOut(writesOf(client), issueKey)) return;
    authoritative.delete(issueKey);
    if (answer === null) {
      void client.invalidateQueries({ queryKey: ["user-state"] });
      return;
    }
    client.setQueryData<UserState>(["user-state"], (current) => ({
      ...current,
      [issueKey]: answer as UserIssueState,
    }));
  },
  name: "pin",
  reset: () => authoritative.clear(),
  resume: (detail) => pinAttempt(detail),
  undone: (client, write) => {
    authoritative.delete(write.detail.issueKey);
    void client.invalidateQueries({ queryKey: ["user-state"] });
  },
});

/** Pins or unpins one item of `issueKey` for the viewer: applied to the page's state at once and
 *  written behind every pin of that issue still out. The operation may be derived from the stored
 *  `dismissed` list at the moment of the write, which is what a toggle needs. */
export function writePin(
  client: QueryClient,
  issueKey: string,
  operation: PinStateOperation | ((dismissed: string[]) => PinStateOperation)
): void {
  let applied: PinStateOperation | undefined;
  client.setQueryData<UserState>(["user-state"], (current) => {
    const state = stateForIssue(current, issueKey);
    applied = typeof operation === "function" ? operation(state.dismissed) : operation;
    return {
      ...current,
      [issueKey]: { ...state, dismissed: applyPinStateOperation(state.dismissed, applied) },
    };
  });
  if (applied === undefined) return;
  const detail = { issueKey, operation: applied };
  writesOf(client)
    .run({
      detail,
      kind: pinWrite,
      label: `${applied.op === "pin" ? "Pin" : "Unpin"} on ${issueKey}`,
      presentation: "indicator",
      target: `pin:${issueKey}`,
      write: pinAttempt(detail),
    })
    .catch(() => {});
}

/** Forgets every issue state a pin answer left: the unit-test preload, between tests. */
export function resetPinState(): void {
  authoritative.clear();
}

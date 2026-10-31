import type { MessageDeliveryMode } from "@legion/contracts";
import type { MutationKey } from "@tanstack/react-query";

import { newSendKey } from "../../lib/send-key";
import { type HeldSend, useHeldSendOutcomes, useHeldSends, useHeldSendsTo } from "./held-sends";

export interface SessionSends {
  /** Every message to the session not yet landed - the live view's presses and its Agents row's -
   *  in press order, each shown as its own pending turn wherever the session is. */
  readonly pending: readonly HeldSend[];
  /** Sends `body` as written in `delivery`, under a key and a name of its own beneath `name`: each
   *  press is its own write, and it goes out behind every earlier message to the session still
   *  being tried. */
  readonly send: (name: MutationKey, body: string, delivery: MessageDeliveryMode) => void;
  /** Sends a refused message again, at the back of the session's line, as every other surface's
   *  Retry does: one body decides what a Retry carries (`HeldSends.retry`). */
  readonly retry: (held: HeldSend) => void;
  readonly discard: (held: HeldSend) => void;
  /** Stops trying a late message once the attempt in flight settles. */
  readonly stop: (held: HeldSend) => void;
}

/** No message to any session: a thread rendered with no store behind it (a test harness). */
export const NO_SESSION_SENDS: SessionSends = {
  discard: () => {},
  pending: [],
  retry: () => {},
  send: () => {},
  stop: () => {},
};

/**
 * The messages to session `sessionId`, as every surface of that session reads and acts on them -
 * its live view and its Agents row. The session is the write's target: each message is a write of
 * its own in the app's write store, ordered on the session (`session:<id>`) in press order, a later
 * one waiting only behind an earlier one still being tried, and either surface's Retry, Discard or
 * Stop trying acts on the same write. Send is pressable whenever the box holds text (INFRA-442): a
 * press never waits on, refuses or replaces an earlier one. A message belongs to its session, not
 * to the surface that made it: leaving either leaves it being tried under its key, and both show
 * it until Dispatch answers, the reader stops it, or the reader discards its refusal. `landed`
 * runs whenever a message to the session lands while the caller is mounted.
 */
export function useSessionSends(sessionId: string, landed?: () => void): SessionSends {
  const store = useHeldSends();
  const pending = useHeldSendsTo(sessionId);
  useHeldSendOutcomes(({ kind, send: { request } }) => {
    const owner = request.owner;
    if (kind === "landed" && owner.kind === "session" && owner.sessionId === sessionId) {
      landed?.();
    }
  });
  return {
    discard: (held) => void store.discard(held.mutationKey),
    pending,
    retry: (held) => store.retry(held.mutationKey),
    send: (name, body, delivery) => {
      const sendKey = newSendKey();
      store.send([...name, sendKey], {
        anchor: undefined,
        ask: { multiple: false, options: [], urgency: "med" },
        draft: { body, mentions: [], replacement: "" },
        edit: undefined,
        kind: "comment",
        owner: { kind: "session", sessionId },
        pickedDelivery: delivery,
        plainDelivery: delivery,
        replyTo: null,
        sendKey,
      });
    },
    stop: (held) => void store.stop(held.mutationKey),
  };
}

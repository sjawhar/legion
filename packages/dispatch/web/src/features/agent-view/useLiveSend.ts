import type { MessageDeliveryMode } from "@legion/contracts";
import { hashKey } from "@tanstack/react-query";
import { useEffect, useMemo, useRef } from "react";

import { newSendKey } from "../../lib/send-key";
import { agentSendKey } from "../agents/AgentMessageComposer";
import { type HeldSend, useHeldSend, useHeldSends } from "../conversation/held-sends";

export interface LiveSendControls {
  /** The send the session holds - from this view or its Agents row - out, or refused. */
  readonly held: HeldSend | undefined;
  /** Sends `body` as written in `delivery` under a key of its own, unless the session holds a
   *  send already, out or refused. */
  readonly send: (body: string, delivery: MessageDeliveryMode) => void;
  /** Sends the refused message again as it was sent, under the key it was sent with. */
  readonly retry: () => void;
  /** Drops a refused message. */
  readonly discard: () => void;
  /** Stops trying a late message once the attempt in flight settles. */
  readonly stop: () => void;
}

/**
 * The live view's message to session `sessionId`, held by the app's held-send store under the
 * session's own send name (`agentSendKey`), the one its Agents row sends under: a send belongs to
 * its session, not to the view that made it. Leaving the view - for another session, another page
 * - leaves it being tried (`sendKeyedAgentMessage`), and the view, or the row, shows it again on
 * return; it ends only when Dispatch answers, the reader stops it, or the reader discards its
 * refusal. One send per name, so a press on either surface sends nothing while the session holds
 * one, and a second message never goes out beside the first under a key of its own. `landed` runs
 * whenever a send held for the session lands while this view is on screen.
 */
export function useLiveSend(sessionId: string, landed: () => void): LiveSendControls {
  const store = useHeldSends();
  const key = useMemo(() => agentSendKey(sessionId), [sessionId]);
  const held = useHeldSend(key);
  const onLanded = useRef(landed);
  onLanded.current = landed;
  useEffect(() => {
    const id = hashKey(key);
    return store.onOutcome(({ kind, send }) => {
      if (kind === "landed" && hashKey(send.mutationKey) === id) onLanded.current();
    });
  }, [key, store]);

  return {
    discard: () => store.discard(key),
    held,
    retry: () => {
      const refused = store.get(key);
      if (refused?.status === "refused") store.send(key, refused.request);
    },
    send: (body, delivery) => {
      // Read in the press's own task, so a press React has not yet rendered as held sends nothing.
      if (store.get(key) !== undefined) return;
      store.send(key, {
        anchor: undefined,
        ask: { multiple: false, options: [], urgency: "med" },
        draft: { body, mentions: [], replacement: "" },
        edit: undefined,
        kind: "comment",
        owner: { kind: "session", sessionId },
        pickedDelivery: delivery,
        plainDelivery: delivery,
        replyTo: null,
        sendKey: newSendKey(),
      });
    },
    stop: () => store.stop(key),
  };
}

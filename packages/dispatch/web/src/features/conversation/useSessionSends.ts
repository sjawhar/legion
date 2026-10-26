import type { MessageDeliveryMode } from "@legion/contracts";
import { hashKey } from "@tanstack/react-query";
import { useEffect, useMemo, useRef } from "react";

import { newSendKey } from "../../lib/send-key";
import { agentSendKey } from "../agents/AgentMessageComposer";
import { type HeldSend, useHeldSend, useHeldSends } from "../conversation/held-sends";
import { requestFor } from "../conversation/send-request";

export interface LiveSendControls {
  /** The send the session holds - from this view or its Agents row - out, or refused. */
  readonly held: HeldSend | undefined;
  /** Sends `body` as written in `delivery` under a key of its own, unless the session holds a
   *  send already, out or refused. */
  readonly send: (body: string, delivery: MessageDeliveryMode) => void;
  /** Sends the refused message again: as it was sent - under its own key when it ended without
   *  Dispatch's verdict, a new one when Dispatch refused it - or, where the draft was written over
   *  since on this session's Agents row, that draft, under a key of its own. */
  readonly retry: () => void;
  readonly discard: () => void;
  /** Stops trying a late message once the attempt in flight settles. */
  readonly stop: () => void;
}

/**
 * The live view's message to session `sessionId`, held by the app's held-send store under the
 * session's own send name (`agentSendKey`), the one its Agents row sends under: a send belongs to
 * its session, not to the view that made it. Leaving the view - for another session, another page
 * - leaves it being tried under its key (`sendRequest`), and the view, or the row, shows it again on
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
      if (refused?.status !== "refused") return;
      // The row and this view share one send, so Retry sends the draft the store holds, which the
      // row's composer writes what the reader typed over the refusal into: unedited, that is the
      // refused request itself, picked mode included - under its key when it ended without
      // Dispatch's verdict, so Dispatch makes one message of the two; edited, it is another
      // message, in the mode this one went in, under a key of its own (`requestFor`).
      const { sendKey: _sent, ...addressed } = refused.request;
      store.send(
        key,
        requestFor(refused.request, refused.refusal, {
          ...addressed,
          ask: refused.draft.ask,
          draft: refused.draft.draft,
        })
      );
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

import { useState } from "react";

import type { CreateBroadcastInput, MessageDeliveryMode } from "../../api/types";
import { newSendKey } from "../../lib/send-key";
import type { BroadcastSend } from "./broadcast-queue";

/** The broadcast being composed on the Agents page. */
export interface BroadcastComposition {
  /** The mode the sender picked, or null until they pick one: the page then sends in the mode
   *  the selection takes (`broadcastDelivery`). */
  readonly delivery: MessageDeliveryMode | null;
  readonly draft: string;
  /** A refused send's whole request, put back by Restore draft and dropped by the first edit. */
  readonly restored: CreateBroadcastInput | null;
  readonly selected: ReadonlySet<string>;
  /** The key the next live send carries; `sent` replaces it. */
  readonly sendKey: string;
  readonly setDelivery: (delivery: MessageDeliveryMode) => void;
  readonly setDraft: (draft: string) => void;
  readonly setSelected: (update: (current: ReadonlySet<string>) => ReadonlySet<string>) => void;
  /** Restore draft: a refused send's message, mode, selection and request come back. */
  readonly restore: (send: BroadcastSend) => void;
  /** Send handed the request to the queue: the composer empties, and the next send gets a key. */
  readonly sent: () => void;
}

type CompositionState = Omit<
  BroadcastComposition,
  "restore" | "sent" | "setDelivery" | "setDraft" | "setSelected"
>;

/**
 * The composition is page state, not the composer's: the composer unmounts whenever the selection
 * empties, and clearing a selection to pick again must not throw away a typed message or its mode.
 * Its only mutators are an edit, a restore and a send, so the rules live here. One key per send
 * (`CreateBroadcastInput.idempotency_key`): the key changes only once Send has handed it to the
 * queue, so two presses of one composition name one broadcast on the server, and the send after
 * it never reuses its key. Every edit - the message, the mode, the selection - drops a restored
 * request: until then Send re-sends it word for word, so the server sees the request it refused
 * even after the registry changed (`composedBroadcast`).
 */
export function useBroadcastComposition(): BroadcastComposition {
  const [state, setState] = useState<CompositionState>(() => ({
    delivery: null,
    draft: "",
    restored: null,
    selected: new Set(),
    sendKey: newSendKey(),
  }));
  return {
    ...state,
    restore: (send) =>
      setState((current) => ({
        ...current,
        delivery: send.picked ? send.input.delivery : null,
        draft: send.input.body,
        restored: send.input,
        selected: new Set(send.selected),
      })),
    sent: () =>
      setState((current) => ({
        ...current,
        draft: "",
        restored: null,
        selected: new Set(),
        sendKey: newSendKey(),
      })),
    setDelivery: (delivery) => setState((current) => ({ ...current, delivery, restored: null })),
    setDraft: (draft) => setState((current) => ({ ...current, draft, restored: null })),
    setSelected: (update) =>
      setState((current) => ({ ...current, restored: null, selected: update(current.selected) })),
  };
}

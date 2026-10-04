import { useState } from "react";

import type { CreateBroadcastInput, MessageDeliveryMode } from "../../api/types";
import type { BroadcastSend } from "./BroadcastSends";

/**
 * A v4 UUID for one send's `idempotency_key`, built from `crypto.getRandomValues`.
 * `crypto.randomUUID` exists only in a secure context, and Dispatch opened over plain HTTP by a
 * non-loopback name is not one, so the page would throw on its first render there.
 */
export function newSendKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/** The broadcast being composed on the Agents page. */
export interface BroadcastComposition {
  readonly delivery: MessageDeliveryMode;
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
    delivery: "btw",
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
        delivery: send.input.delivery,
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

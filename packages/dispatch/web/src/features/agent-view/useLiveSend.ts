import type { MessageDeliveryMode } from "@legion/contracts";
import { useEffect, useRef, useState } from "react";

import { newSendKey } from "../../lib/send-key";
import { SEND_DEADLINE_MS, sendKeyedAgentMessage } from "../conversation/send-request";

/** The live view's message on its way to the session: what was pressed, under the key it was
 *  pressed with, out within the send's deadline (`sending`), out past it (`late`), or refused and
 *  not yet sent again or discarded. */
export interface LiveSend {
  readonly body: string;
  readonly delivery: MessageDeliveryMode;
  readonly key: string;
  readonly status: "late" | "refused" | "sending";
  /** Whether the reader pressed Stop trying and the attempt in flight is still out. */
  readonly stopping: boolean;
  /** Dispatch's refusal, or `SendStoppedError` once the reader stopped trying. */
  readonly refusal: Error | undefined;
}

export interface LiveSendControls {
  /** The message held for this session, if any. */
  readonly held: LiveSend | undefined;
  /** Sends `body` in `delivery` under a key of its own, unless a message is held already. */
  readonly send: (body: string, delivery: MessageDeliveryMode) => void;
  /** Sends a refused message again, under the key it was pressed with. */
  readonly retry: () => void;
  /** Drops a refused message. */
  readonly discard: () => void;
  /** Stops trying a late message once the attempt in flight settles. */
  readonly stop: () => void;
}

/**
 * The live view's one message at a time to session `sessionId`. A press holds its message until
 * Dispatch decides it - shown, and keeping a second press from sending another message beside it,
 * which would carry a key of its own and reach the session twice - and `sendKeyedAgentMessage`
 * tries it under its key until then. Its trying is this view's: leaving the view, for another
 * session or another page, stops it between attempts (the attempt in flight may already have
 * reached Dispatch, so it is let finish), and a view that is gone tells nobody how it ended.
 * `landed` runs once Dispatch confirms a message while its view is still the one on screen.
 */
export function useLiveSend(sessionId: string, landed: () => void): LiveSendControls {
  const [state, setState] = useState<{ readonly sessionId: string; readonly send: LiveSend }>();
  const held = state?.sessionId === sessionId ? state.send : undefined;
  // What the send out now is and what stops it, read in the press's own task, before React has
  // rendered the message it holds.
  const out = useRef<{ readonly key: string; readonly stop: AbortController } | null>(null);
  const view = useRef<AbortController | null>(null);
  const onLanded = useRef(landed);
  onLanded.current = landed;
  // biome-ignore lint/correctness/useExhaustiveDependencies: `sessionId` is the trigger: each session's view stops its own sends
  useEffect(() => {
    const current = new AbortController();
    view.current = current;
    return () => {
      current.abort();
      out.current = null;
      // A message left behind is no longer being tried, so coming back must not show it out.
      setState(undefined);
    };
  }, [sessionId]);

  const run = (message: LiveSend) => {
    const leave = view.current?.signal;
    if (leave === undefined || leave.aborted) return;
    const stop = new AbortController();
    out.current = { key: message.key, stop };
    const update = (change: Partial<LiveSend>) =>
      setState((current) =>
        current?.send.key === message.key
          ? { ...current, send: { ...current.send, ...change } }
          : current
      );
    setState({
      sessionId,
      send: { ...message, refusal: undefined, status: "sending", stopping: false },
    });
    const deadline = setTimeout(() => update({ status: "late" }), SEND_DEADLINE_MS);
    sendKeyedAgentMessage(
      sessionId,
      { body: message.body, delivery: message.delivery },
      message.key,
      AbortSignal.any([leave, stop.signal])
    )
      .then(
        () => {
          if (leave.aborted) return;
          out.current = null;
          setState((current) => (current?.send.key === message.key ? undefined : current));
          onLanded.current();
        },
        (error: unknown) => {
          if (leave.aborted) return;
          out.current = null;
          update({
            refusal: error instanceof Error ? error : new Error("could not reach the session"),
            status: "refused",
            stopping: false,
          });
        }
      )
      .finally(() => clearTimeout(deadline));
  };

  return {
    discard: () => {
      if (held?.status === "refused") setState(undefined);
    },
    held,
    retry: () => {
      if (held?.status === "refused" && out.current === null) run(held);
    },
    send: (body, delivery) => {
      if (held !== undefined || out.current !== null) return;
      run({
        body,
        delivery,
        key: newSendKey(),
        refusal: undefined,
        status: "sending",
        stopping: false,
      });
    },
    stop: () => {
      if (held?.status !== "late" || held.stopping) return;
      out.current?.stop.abort();
      setState((current) =>
        current?.send.key === held.key
          ? { ...current, send: { ...current.send, stopping: true } }
          : current
      );
    },
  };
}

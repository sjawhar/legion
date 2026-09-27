import type { AgentStreamFrame, AgentStreamReplay, AgentStreamResponder } from "@legion/contracts";
import { useEffect, useState } from "react";

import { EventStreamHttpError, readEventStream, reconnectDelayMs } from "../../api/live";
import {
  type AgentConversation,
  applyFrame,
  applyFrames,
  EMPTY_CONVERSATION,
} from "./conversation";

export type AgentStreamStatus = "connecting" | "live" | "reconnecting" | "unavailable";

export interface AgentStreamState {
  readonly conversation: AgentConversation;
  readonly status: AgentStreamStatus;
  /** Whether the session answers the relay on its control subject; `undefined` until it says.
   *  `false` is a session that cannot stream - an older plugin, or one no longer running - and
   *  it can turn true on a later replay without this viewer reconnecting. */
  readonly responding: boolean | undefined;
}

/**
 * Watches one session's own conversation.
 *
 * The stream is not resumable: it carries no cursor and the server keeps no log of it, because
 * nothing about a session's conversation is stored. A reconnect therefore asks the session for
 * its replay again and rebuilds from that, which is exactly what a first connection does.
 */
export function useAgentStream(sessionID: string): AgentStreamState {
  const [state, setState] = useState<AgentStreamState>({
    conversation: EMPTY_CONVERSATION,
    responding: undefined,
    status: "connecting",
  });

  useEffect(() => {
    let stopped = false;
    let attempt = 0;
    let controller: AbortController | null = null;
    let reconnect: number | undefined;
    setState({ conversation: EMPTY_CONVERSATION, responding: undefined, status: "connecting" });

    const open = (): void => {
      const current = new AbortController();
      controller = current;
      void readEventStream(`/api/v1/agents/${encodeURIComponent(sessionID)}/stream`, {
        onChunk: () => undefined,
        onEvent: (raw) => {
          if (stopped) return;
          if (raw.event === "responder") {
            const responder = JSON.parse(raw.data) as AgentStreamResponder;
            setState((previous) => ({ ...previous, responding: responder.responding }));
            return;
          }
          if (raw.event === "replay") {
            const replay = JSON.parse(raw.data) as AgentStreamReplay;
            // A reconnect rebuilds from the session's own replay rather than layering it over
            // a conversation whose newest frames may already be gone from the session's ring.
            setState((previous) => ({
              conversation: applyFrames(EMPTY_CONVERSATION, replay.frames),
              responding: previous.responding,
              status: "live",
            }));
            return;
          }
          if (raw.event !== "frame") return;
          const frame = JSON.parse(raw.data) as AgentStreamFrame;
          setState((previous) => ({
            conversation: applyFrame(previous.conversation, frame),
            // A frame is a session speaking, which is the same evidence the replay answer is.
            responding: true,
            status: "live",
          }));
        },
        onOpen: () => {
          if (stopped) return;
          attempt = 0;
          setState((previous) => ({ ...previous, status: "live" }));
        },
        signal: current.signal,
      }).then(
        () => settle(current, undefined),
        (error: unknown) => settle(current, error)
      );
    };

    const settle = (current: AbortController, error: unknown): void => {
      if (stopped) return;
      controller = null;
      // A relay this deployment does not have, a session id that is not a human's to watch, or a
      // signed-out viewer: reconnecting changes none of them.
      if (!current.signal.aborted && error instanceof EventStreamHttpError && error.status < 500) {
        setState((previous) => ({ ...previous, status: "unavailable" }));
        return;
      }
      setState((previous) => ({ ...previous, status: "reconnecting" }));
      reconnect = window.setTimeout(open, reconnectDelayMs(attempt));
      attempt += 1;
    };

    open();
    return () => {
      stopped = true;
      window.clearTimeout(reconnect);
      controller?.abort();
      controller = null;
    };
  }, [sessionID]);

  return state;
}

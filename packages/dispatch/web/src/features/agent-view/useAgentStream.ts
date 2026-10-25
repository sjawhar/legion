import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, useSyncExternalStore } from "react";

import { liveSocketFor } from "../../api/live-socket";
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
  /** The one signal that the whole transcript was rebuilt from the session's replay, counted up
   *  once per rebuild: the first subscription, every time the socket sends it again (a reconnect,
   *  a rotation, Dispatch ending it), and a new session's first replay. A reader who has scrolled
   *  back is put back on the turn they were reading when it survives the rebuild
   *  (`use-live-follow.ts`), so this has to count every rebuild, not only a reconnect. */
  readonly replays: number;
  /** Whether the session answers the relay on its control subject; `undefined` until it says.
   *  `false` is a session that cannot stream - an older plugin, or one no longer running - and
   *  it can turn true on a later replay without this viewer reconnecting. */
  readonly responding: boolean | undefined;
}

/** What the view has heard on its subscription to one session. */
interface Heard {
  readonly conversation: AgentConversation;
  readonly replays: number;
  readonly responding: boolean | undefined;
  /** Whether any of the session's frames has arrived since the view subscribed. */
  readonly answered: boolean;
  /** Dispatch refused the subscription: a viewer who is not a person, a deployment with no relay,
   *  a session id that is not a UUID, a socket at its cap. Final for this session id: the socket
   *  never sends a refused view again. */
  readonly refused: boolean;
}

const NOTHING_HEARD: Heard = {
  answered: false,
  conversation: EMPTY_CONVERSATION,
  refused: false,
  replays: 0,
  responding: undefined,
};

/**
 * Watches one session's own conversation, as a subscription on the tab's live socket.
 *
 * The stream is not resumable: it carries no cursor and the server keeps no log of it, because
 * nothing about a session's conversation is stored. Each time the socket sends the subscription
 * again, the session is asked for its replay and the view rebuilds from that, which is exactly
 * what a first subscription does.
 */
export function useAgentStream(sessionID: string): AgentStreamState {
  const queryClient = useQueryClient();
  const live = liveSocketFor(queryClient);
  // The connection alone: the rest of the socket's state - the delivery pages' last change time
  // among it - moves with every delivery delta, and none of it is this view's.
  const connection = useSyncExternalStore(live.onState, () => live.state().state);
  const [heard, setHeard] = useState<Heard>(NOTHING_HEARD);

  useEffect(() => {
    // A new session starts its own count: its first replay is a rebuild of this viewer's
    // transcript as much as a reconnect's is.
    setHeard(NOTHING_HEARD);
    // The socket calls none of these handlers once the unsubscribe returned here has run.
    return liveSocketFor(queryClient).subscribeAgent(sessionID, {
      onResponder: (responder) =>
        setHeard((previous) => ({ ...previous, answered: true, responding: responder.responding })),
      // A rebuild from the session's own replay rather than a layer over a conversation whose
      // newest frames may already be gone from the session's ring.
      onReplay: (replay) =>
        setHeard((previous) => ({
          ...previous,
          answered: true,
          conversation: applyFrames(EMPTY_CONVERSATION, replay.frames),
          replays: previous.replays + 1,
        })),
      onFrame: (frame) =>
        setHeard((previous) => ({
          ...previous,
          answered: true,
          conversation: applyFrame(previous.conversation, frame),
          // A frame is a session speaking, which is the same evidence the responder answer is.
          responding: true,
        })),
      onUnavailable: () => setHeard((previous) => ({ ...previous, refused: true })),
    });
  }, [queryClient, sessionID]);

  // A refusal outranks the socket's state; a lost socket outranks what the view last heard.
  const status: AgentStreamStatus = heard.refused
    ? "unavailable"
    : connection === "reconnecting"
      ? "reconnecting"
      : heard.answered
        ? "live"
        : "connecting";
  return {
    conversation: heard.conversation,
    replays: heard.replays,
    responding: heard.responding,
    status,
  };
}

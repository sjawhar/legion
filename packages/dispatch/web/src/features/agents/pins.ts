/**
 * The agents a person has pinned (INFRA-812, R1).
 *
 * A pin belongs to the person, not to the browser it was made in: it is the session's
 * `pinned_at` in the viewer's per-agent state (`userAgentStateQuery`), written through
 * `PUT /api/v1/me/agents/{session}/state` (`agentStateRequest`). Every surface that shows or
 * moves a pin - the Agents page's row, the live view's header and the rail's Agents list - reads
 * and writes it here, so a pin made in one shows in the others at once, and in another browser
 * of the same person without a reload, from the `user_agent_state.updated` event that write
 * announces (`api/event-refresh.ts`).
 */
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";

import { agentStateRequest } from "../../api/client";
import { userAgentStateQuery } from "../../api/queries";
import type { UserAgentState, UserAgentStates } from "../../api/types";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
import { useAgentText } from "../refs/AgentName";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { storeAgentState } from "./unread";

/** One press of a pin control: the session, and what the press makes of it. */
interface PinPress {
  readonly pinned: boolean;
  readonly sessionId: string;
}

/** One pin, as the rail orders them. */
interface Pin {
  readonly at: string;
  readonly sessionId: string;
}

/** Orders two pins oldest first. A `pinned_at` this page cannot read - no Dispatch writes one,
 *  but a value the ordering threw on would take the whole rail with it - goes last, and two
 *  pins of one instant are ordered by session so the rail never swaps two rows on a re-render. */
function comparePins(left: Pin, right: Pin): number {
  const leftRead = isTimestamp(left.at);
  const rightRead = isTimestamp(right.at);
  if (leftRead && rightRead) {
    const order = compareTimestamps(left.at, right.at);
    if (order !== 0) return order;
  } else if (leftRead !== rightRead) {
    return leftRead ? -1 : 1;
  }
  return left.sessionId < right.sessionId ? -1 : 1;
}

/** The sessions the viewer has pinned, oldest pin first: the rail's Agents list, and the Agents
 *  page's pin order. A session the viewer has only read, or cleared, carries no `pinned_at` and
 *  is not among them. */
export function pinnedAgentSessions(states: UserAgentStates | undefined): string[] {
  const pins: Pin[] = [];
  for (const [sessionId, state] of Object.entries(states ?? {})) {
    if (state.pinned_at !== undefined) pins.push({ at: state.pinned_at, sessionId });
  }
  return pins.sort(comparePins).map((pin) => pin.sessionId);
}

/** The viewer's pins, oldest first, re-read whenever their agent state changes. */
export function usePinnedAgents(): readonly string[] {
  const states = useQuery(userAgentStateQuery()).data;
  return useMemo(() => pinnedAgentSessions(states), [states]);
}

/** One session's state as a pin or an unpin leaves it, touching neither its Clear nor its read
 *  mark: the page shows the pin from the press, and Dispatch's answer replaces it. */
function withPin(state: UserAgentState | undefined, pinned: boolean, at: string): UserAgentState {
  const { pinned_at: _was, ...rest } = state ?? {};
  return pinned ? { ...rest, pinned_at: at } : rest;
}

/**
 * Pins or unpins one session for the viewer: `pin(sessionId, true)` from the Agents page's row
 * or the live view's header, `pin(sessionId, false)` from either of those or from the rail's own
 * row. The page shows the press at once and Dispatch's answer replaces it; the write is one
 * write of the one store, ordered on its session (`pin:agent:<id>`), named in the shell's
 * pending-writes indicator and, refused, retried or discarded there, as an issue's pin is. A
 * press whose write fails has the viewer's agent state read again, so no row is left pinned in a
 * page Dispatch never took the pin for.
 */
export function useAgentPinWrite(): (sessionId: string, pinned: boolean) => void {
  const queryClient = useQueryClient();
  const text = useAgentText();
  const reread = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: userAgentStateQuery().queryKey });
  }, [queryClient]);
  const { mutate } = useKeyedWrite<UserAgentState, PinPress>({
    label: ({ pinned, sessionId }) =>
      `${pinned ? "Pin" : "Unpin"} ${text.sessionText(sessionId, undefined)}`,
    onError: reread,
    onMutate: ({ pinned, sessionId }) => {
      const at = new Date().toISOString();
      queryClient.setQueryData<UserAgentStates>(userAgentStateQuery().queryKey, (current) => ({
        ...current,
        [sessionId]: withPin(current?.[sessionId], pinned, at),
      }));
    },
    onSuccess: (next, { sessionId }) => {
      // A write Dispatch made and has no answer to give for is a landing: what it changed is
      // read again rather than left as the press drew it.
      if (next === null) reread();
      else storeAgentState(queryClient, sessionId, next);
    },
    request: ({ pinned, sessionId }) => agentStateRequest(sessionId, { pinned }),
    target: ({ sessionId }) => `pin:agent:${sessionId}`,
  });
  return useCallback(
    (sessionId: string, pinned: boolean) => mutate({ pinned, sessionId }),
    [mutate]
  );
}

/**
 * The session ids a browser kept under the old `dispatch.agents.pinned:<login>` key, which the
 * shell migrates once (`app.tsx`, `legacyPinnedAgentsKey`). Anything but an array of strings was
 * never written by that preference, so it carries no pins.
 */
export function legacyPinnedIds(stored: string | null): string[] {
  if (stored === null) return [];
  try {
    const parsed: unknown = JSON.parse(stored);
    return Array.isArray(parsed) ? parsed.filter((value) => typeof value === "string") : [];
  } catch {
    return [];
  }
}

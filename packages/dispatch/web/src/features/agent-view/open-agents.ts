/** The agents a person has open in the live view's column, and the divider's share of the pane
 *  area beside it. Both belong to this browser under the signed-in login, so both are
 *  `useUserPreference` values (`packages/dispatch/web/src/features/shell/userPreference.ts`).
 *
 *  Which pane holds which agent is deliberately not here and is never stored: the address owns
 *  it, as `/agents/<left>/live?beside=<right>` read by `parseAgentLiveSearch` in
 *  `packages/dispatch/web/src/features/refs/routes.ts`, so that a link opens the same two panes
 *  and a reload restores them. A stored pane would be a second answer to that question, and the
 *  two would disagree the moment a link was followed; hence no pane field and no `move`.
 *
 *  Each agent's unsent draft and reading place are not here either: they are the live view's own
 *  state, kept for as long as a view is on screen rather than as a browser preference. */

import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useRef } from "react";

import { whoAmIQuery } from "../../api/queries";
import { userPreferenceStorageKey, useUserPreference } from "../shell/userPreference";
import { DEFAULT_PANE_SHARE } from "./workspace-width";

/** The stored list, in the shape the pinned list is read in
 *  (`packages/dispatch/web/src/features/agents/AgentsPage.tsx`): anything that is not a JSON
 *  array of session ids answers the empty list rather than propagating into the column. */
function readOpenAgents(stored: string | null): readonly string[] {
  try {
    const parsed: unknown = stored === null ? [] : JSON.parse(stored);
    return Array.isArray(parsed)
      ? parsed.filter((value): value is string => typeof value === "string")
      : [];
  } catch {
    return [];
  }
}

function writeOpenAgents(open: readonly string[]): string {
  return JSON.stringify(open);
}

export interface OpenAgents {
  /** The session ids in the person's own order. */
  open: readonly string[];
  openAgent: (sessionId: string) => void;
  closeAgent: (sessionId: string) => void;
  reorder: (from: number, to: number) => void;
}

export function useOpenAgents(): OpenAgents {
  const login = useQuery(whoAmIQuery()).data?.login;
  const [open, setOpen] = useUserPreference("agents.open", readOpenAgents, writeOpenAgents);
  // The list as the last call left it. `useUserPreference`'s setter takes a value rather than an
  // updater, so without this a view that opens both of its panes at once would write the second
  // session over the first: both calls read the same render's `open`.
  const listRef = useRef(open);
  listRef.current = open;
  // Sessions opened while the login was not yet known, in the order they were opened. See the
  // effect below.
  const openedBeforeLogin = useRef<readonly string[]>([]);
  const update = useCallback(
    (next: readonly string[]) => {
      listRef.current = next;
      setOpen(next);
    },
    [setOpen]
  );
  const openAgent = useCallback(
    (sessionId: string) => {
      // A session already open stays exactly where it is: opening it again is a person returning
      // to a view they have, not a new row.
      if (listRef.current.includes(sessionId)) return;
      if (login === undefined) {
        openedBeforeLogin.current = [...openedBeforeLogin.current, sessionId];
      }
      update([...listRef.current, sessionId]);
    },
    [login, update]
  );
  const closeAgent = useCallback(
    (sessionId: string) => {
      // Closed again before the identity resolved: nothing to write back for it.
      openedBeforeLogin.current = openedBeforeLogin.current.filter((id) => id !== sessionId);
      if (!listRef.current.includes(sessionId)) return;
      // Only this list. Closing a view leaves the agent pinned: `agents.pinned`
      // (`packages/dispatch/web/src/features/agents/AgentsPage.tsx`) is the person's own mark on
      // a session, which no window being shut may revoke.
      update(listRef.current.filter((id) => id !== sessionId));
    },
    [update]
  );
  const reorder = useCallback(
    (from: number, to: number) => {
      const list = listRef.current;
      if (!Number.isInteger(from) || !Number.isInteger(to)) return;
      if (from < 0 || from >= list.length || to < 0 || to >= list.length) return;
      const moved = list[from];
      const rest = list.filter((_, index) => index !== from);
      update([...rest.slice(0, to), moved, ...rest.slice(to)]);
    },
    [update]
  );
  // `userPreference.ts`: a value written while the signed-in identity is pending or failed does
  // not persist, and the hook re-reads storage when that identity resolves. A person who lands
  // straight on `/agents/<a>/live?beside=<b>` opens both rows before `/auth/whoami` answers, so
  // without this cure the resolving identity would drop both. Storage is read here rather than
  // from `open`, because the hook's own re-read and this effect run in the same commit and this
  // one has to win.
  useEffect(() => {
    if (login === undefined) return;
    const opened = openedBeforeLogin.current;
    if (opened.length === 0) return;
    openedBeforeLogin.current = [];
    const stored = readOpenAgents(
      window.localStorage.getItem(userPreferenceStorageKey(login, "agents.open"))
    );
    const merged = [...stored];
    for (const sessionId of opened) {
      if (!merged.includes(sessionId)) merged.push(sessionId);
    }
    update(merged);
  }, [login, update]);
  return { closeAgent, open, openAgent, reorder };
}

/** The stored share is read from text, so text that is no number at all answers the default
 *  rather than a NaN, and a share from either direction is held inside the fraction a divider
 *  can stand at. The pane floors are a separate bound, applied against a measured width by
 *  `clampPaneShare` in `./workspace-width`. */
function paneShareInRange(share: number): number {
  if (!Number.isFinite(share)) return DEFAULT_PANE_SHARE;
  return Math.min(Math.max(share, 0), 1);
}

function readPaneShare(stored: string | null): number {
  return stored === null ? DEFAULT_PANE_SHARE : paneShareInRange(Number(stored));
}

export function usePaneShare(): [number, (share: number) => void] {
  const [share, setShare] = useUserPreference("agents.pane-share", readPaneShare, String);
  const setShareInRange = useCallback(
    (next: number) => setShare(paneShareInRange(next)),
    [setShare]
  );
  return [share, setShareInRange];
}

import { queryOptions, useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useSyncExternalStore } from "react";

import { api, isAgentListRefilling } from "../../api/client";
import type { Agent } from "../../api/types";

/**
 * The agent registry (`GET /api/v1/agents`: the sessions the Envoy listener lists right now) as
 * every surface reads it. One query key, one lookup built per response, and one poller:
 * `AgentRegistryPoller`, mounted once in the app shell, refreshes the registry every 15 s while
 * anything registered through `useRegistryPolling` is mounted (each session `AgentName`, each
 * `useAgents(…, true)` caller). A reader never polls on its own: TanStack runs one interval
 * per observer, so N readers mounted at different times would fetch up to N times per 15 s.
 * The poller keeps TanStack's default of not polling a hidden tab.
 */

/** A session's running state, from the registry only. `running`: listed by the latest
 *  successful read. `not-running`: the latest read succeeded and does not list it. `unknown`:
 *  no successful read yet, or the latest read failed. */
export type AgentState = "running" | "not-running" | "unknown";

/** The poller's cadence while anything shows an agent. */
export const AGENT_REGISTRY_POLL_MS = 15_000;

/** The registry query every reader shares. */
export const agentsQuery = () =>
  queryOptions({
    queryKey: ["agents"],
    queryFn: () => api.listAgents(),
    staleTime: 10_000,
  });

interface RegistryLookup {
  readonly byId: ReadonlyMap<string, Agent>;
  readonly titles: ReadonlyMap<string, string>;
}

export const EMPTY_AGENTS: readonly Agent[] = [];
const lookups = new WeakMap<readonly Agent[], RegistryLookup>();

/** The lookup for one registry response, built once and shared by every reader of it. */
function registryLookup(agents: readonly Agent[]): RegistryLookup {
  let lookup = lookups.get(agents);
  if (lookup === undefined) {
    lookup = {
      byId: new Map(agents.map((agent) => [agent.session_id, agent])),
      titles: new Map(agents.map((agent) => [agent.session_id, agent.title])),
    };
    lookups.set(agents, lookup);
  }
  return lookup;
}

let pollingReaders = 0;
const pollingListeners = new Set<() => void>();

function notifyPollingListeners(): void {
  for (const listener of pollingListeners) {
    listener();
  }
}

function subscribePolling(listener: () => void): () => void {
  pollingListeners.add(listener);
  return () => {
    pollingListeners.delete(listener);
  };
}

function pollingWanted(): boolean {
  return pollingReaders > 0;
}

/** Keeps the shell's `AgentRegistryPoller` refreshing the registry while `active` and mounted. */
export function useRegistryPolling(active: boolean): void {
  useEffect(() => {
    if (!active) {
      return;
    }
    pollingReaders += 1;
    notifyPollingListeners();
    return () => {
      pollingReaders -= 1;
      notifyPollingListeners();
    };
  }, [active]);
}

/** The one owner of the registry's refresh cadence, mounted once in the app shell. */
export function AgentRegistryPoller(): null {
  const wanted = useSyncExternalStore(subscribePolling, pollingWanted, pollingWanted);
  useQuery({
    ...agentsQuery(),
    enabled: wanted,
    refetchInterval: wanted ? AGENT_REGISTRY_POLL_MS : false,
    // Coming back on after a pause refetches only data at least one poll old, so a reader that
    // remounts moments after the last one unmounted does not add a request inside the cadence.
    staleTime: AGENT_REGISTRY_POLL_MS,
  });
  return null;
}

export interface AgentRegistry {
  /** The session's running state (`AgentState`). */
  state(sessionId: string): AgentState;
  /** The session's current registry title, when listed with a non-blank one. */
  title(sessionId: string): string | undefined;
  /** When the listener last heard the session (epoch ms), when listed. */
  lastSeen(sessionId: string): number | undefined;
  /** Every listed session's registry title, for the contracts naming functions. */
  readonly titles: ReadonlyMap<string, string>;
  /** The latest read failed: every state is `unknown`, and a view that would judge an absence
   *  says it could not check instead. */
  readonly isError: boolean;
  /** The failed read's reason was Envoy answering that its agent list is refilling
   *  (`isAgentListRefilling`), which a view says in place of an unreachable listener. */
  readonly refilling: boolean;
}

/**
 * Reads the registry. `enabled` fetches it (a first load only: it never refetches on mount, the
 * shell's poller refreshes it) and registers with the poller; disabled, it still reads whatever
 * the cache holds, so its titles name agents but its states stay `unknown` until a read lands.
 */
export function useAgentRegistry(enabled: boolean): AgentRegistry {
  useRegistryPolling(enabled);
  const { data, error, isError, isPending } = useQuery({
    ...agentsQuery(),
    enabled,
    refetchOnMount: false,
  });
  const lookup = registryLookup(data ?? EMPTY_AGENTS);
  // A failed refetch keeps the last response's data, so the state goes back to unknown while
  // the cached titles go on naming agents.
  const known = !(isPending || isError);
  const refilling = isError && isAgentListRefilling(error);
  return useMemo(
    () => ({
      isError,
      lastSeen: (sessionId: string) => lookup.byId.get(sessionId)?.last_seen,
      refilling,
      state: (sessionId: string): AgentState =>
        !known ? "unknown" : lookup.byId.has(sessionId) ? "running" : "not-running",
      title: (sessionId: string) => {
        const title = lookup.byId.get(sessionId)?.title.trim();
        return title === undefined || title === "" ? undefined : title;
      },
      titles: lookup.titles,
    }),
    [isError, known, lookup, refilling]
  );
}

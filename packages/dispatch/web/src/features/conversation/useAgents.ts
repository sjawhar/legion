import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api, isAgentListRefilling } from "../../api/client";
import type { Agent } from "../../api/types";

const EMPTY_AGENTS: readonly Agent[] = [];

/**
 * The live agent list. `agents` keeps the rows of the last successful read through a failed
 * refresh, so a view need not unmount what it shows; but only a `current` list - its latest
 * read succeeded - says who is absent. Every claim that an agent is gone (ended, offline, not
 * matching, missing a capability, holding a lapsed claim) waits for `current`. `refilling` is
 * the failed read's reason when Envoy answered that its list is refilling (`isAgentListRefilling`),
 * which a view says in place of an unreachable listener.
 */
export function useAgents(
  enabled: boolean,
  refreshWhileOpen = false
): {
  agents: readonly Agent[];
  current: boolean;
  error: string | undefined;
  isError: boolean;
  isPending: boolean;
  refetch: () => Promise<unknown>;
  refilling: boolean;
  titles: ReadonlyMap<string, string>;
} {
  const { data, error, isError, isPending, refetch } = useQuery({
    enabled,
    queryFn: () => api.listAgents(),
    queryKey: ["agents"],
    refetchInterval: refreshWhileOpen ? 15_000 : false,
    staleTime: 10_000,
  });
  const agents = data ?? EMPTY_AGENTS;
  const titles = useMemo(
    () => new Map(agents.map((agent) => [agent.session_id, agent.title])),
    [agents]
  );
  const current = !isError && data !== undefined;
  const refilling = isError && isAgentListRefilling(error);

  return useMemo(
    () => ({
      agents,
      current,
      error: error instanceof Error ? error.message : isError ? "network error" : undefined,
      isError,
      isPending,
      refetch,
      refilling,
      titles,
    }),
    [agents, current, error, isError, isPending, refetch, refilling, titles]
  );
}

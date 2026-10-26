import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import type { Agent } from "../../api/types";
import {
  agentsQuery,
  EMPTY_AGENTS,
  registryLookup,
  useRegistryPolling,
} from "../refs/agent-registry";

/** The agent registry list. `refreshWhileOpen` keeps it fresh through the shell's one poller
 *  (`AgentRegistryPoller`) while this caller is mounted and enabled, instead of an interval of
 *  its own. */
export function useAgents(
  enabled: boolean,
  refreshWhileOpen = false
): {
  agents: readonly Agent[];
  error: string | undefined;
  isError: boolean;
  isPending: boolean;
  refetch: () => Promise<unknown>;
  titles: ReadonlyMap<string, string>;
} {
  useRegistryPolling(enabled && refreshWhileOpen);
  const { data, error, isError, isPending, refetch } = useQuery({
    ...agentsQuery(),
    enabled,
    // The poller refreshes what a polled caller reads; refetching it again on mount would put
    // a second request inside the poller's 15 s.
    refetchOnMount: !refreshWhileOpen,
  });
  const agents = data ?? EMPTY_AGENTS;
  const { titles } = registryLookup(agents);

  return useMemo(
    () => ({
      agents,
      error: error instanceof Error ? error.message : isError ? "network error" : undefined,
      isError,
      isPending,
      refetch,
      titles,
    }),
    [agents, error, isError, isPending, refetch, titles]
  );
}

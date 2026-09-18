import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import type { Agent } from "../../api/types";

const EMPTY_AGENTS: readonly Agent[] = [];

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
  const { data, error, isError, isPending, refetch } = useQuery({
    enabled,
    queryFn: () => api.listAgents(),
    queryKey: ["agents"],
    refetchInterval: refreshWhileOpen ? 15_000 : false,
    retry: false,
    staleTime: 10_000,
  });
  const agents = data ?? EMPTY_AGENTS;
  const titles = useMemo(
    () => new Map(agents.map((agent) => [agent.session_id, agent.title])),
    [agents]
  );

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

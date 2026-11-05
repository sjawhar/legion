import { useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "../../api/client";
import type { Subscriber } from "../../api/types";
import { useAgentText } from "../refs/AgentName";
import { type KeyedWriteResult, useKeyedWrite } from "../writes/useKeyedWrite";

/** The sessions subscribed to an issue, and the write that takes one off. The panel's People
 *  section draws both (dispatch://INFRA-1196 R12). */
export interface IssueSubscribers {
  readonly error: boolean;
  readonly fetching: boolean;
  readonly refetch: () => void;
  readonly rows: readonly Subscriber[];
  readonly unsubscribe: KeyedWriteResult<void, string, unknown>;
}

export function useIssueSubscribers(issueKey: string): IssueSubscribers {
  const queryClient = useQueryClient();
  const agentText = useAgentText();
  const subscribers = useQuery({
    queryKey: ["subscribers", issueKey],
    queryFn: () => api.getIssueSubscribers(issueKey),
  });
  const rows = subscribers.isError ? [] : (subscribers.data ?? []);
  const unsubscribe = useKeyedWrite({
    write: (sessionId: string, bounds) => api.unsubscribeIssueSession(issueKey, sessionId, bounds),
    inline: true,
    label: (sessionId) =>
      `${agentText.sessionText(sessionId, rows.find((row) => row.session_id === sessionId)?.title)} unsubscribed from ${issueKey}`,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["subscribers", issueKey] });
    },
  });
  return {
    error: subscribers.isError,
    fetching: subscribers.isFetching,
    refetch: () => {
      void subscribers.refetch();
    },
    rows,
    unsubscribe,
  };
}

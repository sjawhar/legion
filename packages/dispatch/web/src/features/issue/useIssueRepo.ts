import { useQueryClient } from "@tanstack/react-query";

import type { IssueDetails, IssueSummary } from "../../api/types";
import { type IssueFieldWrite, projectListsKey, useIssueFieldWrite } from "./useIssueFieldWrite";

/**
 * The one way the SPA writes an issue's own repository (owner/name, or null to inherit its
 * parent's or the project's default). The pick shows optimistically in the issue detail and every
 * project issue list; an own repository is also what the issue resolves to, while an inherited one
 * is left to the server's answer, which `useIssueFieldWrite` merges.
 */
export function useIssueRepo(issueKey: string): IssueFieldWrite<string | null> {
  const queryClient = useQueryClient();
  const show = (repo: string | null) => {
    const fields = repo === null ? { repo } : { repo, resolved_repo: repo };
    queryClient.setQueryData<IssueDetails>(["issue", issueKey], (current) =>
      current === undefined ? undefined : { ...current, ...fields }
    );
    queryClient.setQueriesData<IssueSummary[]>({ queryKey: projectListsKey }, (current) =>
      current?.map((issue) => (issue.key === issueKey ? { ...issue, ...fields } : issue))
    );
  };
  return useIssueFieldWrite<string | null>(issueKey, { patch: (repo) => ({ repo }), show });
}

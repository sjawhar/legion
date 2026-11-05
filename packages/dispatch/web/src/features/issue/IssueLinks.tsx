import { keepPreviousData, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { api } from "../../api/client";
import { QueryError } from "../../components/QueryError";
import { linkText, textMutedOnSurface, textSecondaryOnSurface } from "../../theme/classes";
import { buildReferencePath, parseNodeReference } from "../refs/routes";

// The header already shows these relations from the issue's own fields: `child_of` as its
// Parent line and `affects` as its Components line. Links lists every other edge.
const shownByTheHeader: Record<string, true> = { child_of: true, affects: true };

/** The issue's outbound edges the rest of the header does not already show, as one line. An
 *  issue with none, or whose read has not answered yet, renders nothing, so the header keeps
 *  its height; a failed read says so and offers Retry. */
export function IssueLinks({
  issueKey,
  version,
}: {
  issueKey: string;
  version: number;
}): ReactNode {
  // An issue write changes its sequence and so this key; the line on screen stays until the new
  // read answers rather than vanishing and coming back.
  const query = useQuery({
    placeholderData: keepPreviousData,
    queryKey: ["issue-links", issueKey, version],
    queryFn: () => api.getIssueLinks(issueKey),
  });
  if (query.isError) {
    return (
      <section aria-label="Links" className="mt-1 text-sm">
        <QueryError
          message="Could not read links."
          onRetry={() => void query.refetch()}
          retrying={query.isFetching}
        />
      </section>
    );
  }
  const edges = query.data?.edges.filter((edge) => shownByTheHeader[edge.kind] !== true) ?? [];
  if (edges.length === 0) {
    return null;
  }
  return (
    <section
      aria-label="Links"
      className={`mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-sm ${textSecondaryOnSurface}`}
    >
      <span className="font-medium">Links:</span>
      <ul className="contents">
        {edges.map((edge) => {
          const route = edge.node.ref === undefined ? undefined : parseNodeReference(edge.node.ref);
          return (
            <li key={`${edge.kind}:${edge.node.kind}:${edge.node.id}`}>
              <span className={textMutedOnSurface}>{edge.kind} </span>
              {route === undefined ? (
                edge.node.id
              ) : (
                <Link className={`underline ${linkText}`} to={buildReferencePath(route)}>
                  {edge.node.kind === "issue"
                    ? edge.node.id
                    : `${edge.node.kind} · ${edge.node.issue_key ?? edge.node.id}`}
                </Link>
              )}
            </li>
          );
        })}
      </ul>
    </section>
  );
}

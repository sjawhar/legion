import { useInfiniteQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { api } from "../../api/client";
import type { PersonNodes } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

export function PersonPage(): ReactNode {
  const { login = "" } = useParams();
  useDocumentTitle(`${login} · Dispatch`);
  const nodes = useInfiniteQuery({
    queryKey: ["person-nodes", login],
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.listPersonNodes(login, { limit: 50, offset: pageParam }),
    getNextPageParam: (last: PersonNodes) =>
      last.offset + last.rows.length < last.total ? last.offset + last.rows.length : undefined,
  });
  const rows = nodes.data?.pages.flatMap((page) => page.rows) ?? [];
  return (
    <section>
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/">
          ← Inbox
        </Link>
        <h1 className="mt-1 text-[22px] font-semibold tracking-tight">
          {nodes.data?.pages[0]?.person ?? login}
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Authored by this person, delivered to their recorded sessions, or mentioning them, newest
          first.
        </p>
      </header>
      {nodes.isPending ? <LoadingSkeleton label="Loading person nodes" /> : null}
      {nodes.isError ? (
        <p className={dangerText}>Could not load person nodes: {nodes.error.message}</p>
      ) : null}
      {nodes.isSuccess && rows.length === 0 ? (
        <EmptyState
          label="Person nodes empty state"
          message="No authored or received nodes are recorded."
        />
      ) : null}
      <ul aria-label="Person nodes" className="space-y-3">
        {rows.map((row) => (
          <li
            className={`rounded-xl border p-3 ${card} ${borderDefault}`}
            key={`${row.kind}:${row.id}`}
          >
            <div className={`flex flex-wrap items-center gap-2 text-xs ${textMutedOnCanvas}`}>
              <Timestamp at={row.created_at} />
              <span>{row.kind}</span>
              {row.relations.map((relation) => (
                <LabelPill key={relation}>
                  {relation === "authored_by"
                    ? "Authored"
                    : relation === "mentions"
                      ? "Mentioned in"
                      : "Received"}
                </LabelPill>
              ))}
            </div>
            <MarkdownPreview className="mt-2 text-sm" fullTitle lines={2} markdown={row.title} />
            {row.href === null ? null : (
              <Link
                className={`mt-2 inline-block min-h-11 py-2 text-sm ${linkText} ${linkHoverText}`}
                to={row.href}
              >
                Open
              </Link>
            )}
          </li>
        ))}
      </ul>
      {nodes.hasNextPage ? (
        <button
          className={`mt-4 min-h-11 rounded-lg border px-3 py-2 text-sm ${borderDefault} ${linkText} ${linkHoverText}`}
          disabled={nodes.isFetchingNextPage}
          onClick={() => void nodes.fetchNextPage()}
          type="button"
        >
          {nodes.isFetchingNextPage ? "Loading…" : "Load older"}
        </button>
      ) : null}
    </section>
  );
}

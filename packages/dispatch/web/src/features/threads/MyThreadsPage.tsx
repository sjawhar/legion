import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { MyThreads } from "../../api/types";
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

const causeLabels = {
  authored: "Wrote in",
  mentioned: "Mentioned",
  addressed: "Addressed",
} as const;

export function MyThreadsPage(): ReactNode {
  useDocumentTitle("My threads · Dispatch");
  const login = useQuery(whoAmIQuery()).data?.login;
  const threads = useInfiniteQuery({
    queryKey: ["my-threads", login],
    enabled: login !== undefined,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.listMyThreads({ limit: 50, offset: pageParam }),
    getNextPageParam: (last: MyThreads) =>
      last.offset + last.rows.length < last.total ? last.offset + last.rows.length : undefined,
  });
  const rows = threads.data?.pages.flatMap((page) => page.rows) ?? [];
  return (
    <section>
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/">
          ← Inbox
        </Link>
        <h1 className="mt-1 text-[22px] font-semibold tracking-tight">My threads</h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Threads you wrote in, were mentioned in or were addressed in. Newest activity first,
          including threads you have read.
        </p>
      </header>
      {threads.isPending ? <LoadingSkeleton label="Loading my threads" /> : null}
      {threads.isError ? (
        <p className={dangerText}>Could not load my threads: {threads.error.message}</p>
      ) : null}
      {threads.isSuccess && rows.length === 0 ? (
        <EmptyState
          label="My threads empty state"
          message="No participation in threads is recorded yet."
        />
      ) : null}
      <ul aria-label="My threads" className="space-y-3">
        {rows.map((row) => (
          <li
            className={`rounded-xl border p-3 ${card} ${borderDefault}`}
            key={`${row.kind}:${row.thread_id}`}
          >
            <div className={`flex flex-wrap items-center gap-2 text-xs ${textMutedOnCanvas}`}>
              <Timestamp at={row.activity_at} />
              <span>{row.kind}</span>
              {row.unread ? <LabelPill>Unread</LabelPill> : null}
              {row.causes.map((cause) => (
                <LabelPill key={cause}>{causeLabels[cause]}</LabelPill>
              ))}
              {row.issue_key ? <span>{row.issue_key}</span> : null}
            </div>
            <MarkdownPreview className="mt-2 text-sm" fullTitle lines={2} markdown={row.subject} />
            <Link
              className={`mt-2 inline-block min-h-11 py-2 text-sm ${linkText} ${linkHoverText}`}
              to={row.href}
            >
              Open
            </Link>
          </li>
        ))}
      </ul>
      {threads.hasNextPage ? (
        <button
          className={`mt-4 min-h-11 rounded-lg border px-3 py-2 text-sm ${borderDefault} ${linkText} ${linkHoverText}`}
          disabled={threads.isFetchingNextPage}
          onClick={() => void threads.fetchNextPage()}
          type="button"
        >
          {threads.isFetchingNextPage ? "Loading…" : "Load older"}
        </button>
      ) : null}
    </section>
  );
}

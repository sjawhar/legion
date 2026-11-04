import { useInfiniteQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { UnifiedInboxPage as InboxPage } from "../../api/types";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { compareTimestamps } from "../../lib/timestamps";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

const labels = {
  ask: "Question",
  thread_reply: "Thread reply",
  direct_reply: "Direct reply",
  mention: "Mention · added to preview",
} as const;

export function UnifiedInboxPage(): ReactNode {
  useDocumentTitle("Unified inbox preview · Dispatch");
  const [mentions, setMentions] = useState(false);
  const inbox = useInfiniteQuery({
    queryKey: ["unified-inbox", mentions],
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.listUnifiedInbox({ limit: 50, offset: pageParam, mentions }),
    getNextPageParam: (last: InboxPage) =>
      last.offset + last.rows.length < last.total ||
      (mentions && last.expansions.length === last.limit)
        ? last.offset + last.limit
        : undefined,
  });
  const seen = new Set<string>();
  const rows = (inbox.data?.pages.flatMap((page) => [...page.rows, ...page.expansions]) ?? [])
    .filter(
      (row) =>
        !seen.has(`${row.cause}:${row.kind}:${row.id}`) &&
        seen.add(`${row.cause}:${row.kind}:${row.id}`)
    )
    .sort((a, b) => compareTimestamps(b.created_at, a.created_at) || a.id.localeCompare(b.id));
  return (
    <section>
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <h1 className="text-[22px] font-semibold tracking-tight">Unified inbox · preview</h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Your questions, thread replies and direct replies together. The current inbox has not
          changed.
        </p>
        <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-sm">
          <Link className={`${linkText} ${linkHoverText}`} to="/">
            Return to current inbox
          </Link>
          <Link className={`${linkText} ${linkHoverText}`} to="/inbox/shadow">
            Inbox comparison
          </Link>
        </div>
        <label className="mt-3 flex min-h-11 items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={mentions}
            onChange={(event) => setMentions(event.target.checked)}
          />
          Include mentions (additional items)
        </label>
      </header>
      {inbox.isPending ? <LoadingSkeleton label="Loading unified inbox" /> : null}
      {inbox.isError ? (
        <p role="alert" className={dangerText}>
          Could not load unified inbox: {inbox.error.message}
        </p>
      ) : null}
      {inbox.isSuccess && rows.length === 0 ? <p>Nothing new in your unified inbox.</p> : null}
      <ul aria-label="Unified inbox" className="space-y-3">
        {rows.map((row) => (
          <li
            className={`rounded-xl border p-3 ${card} ${borderDefault}`}
            key={`${row.cause}:${row.kind}:${row.id}`}
          >
            <div className={`flex flex-wrap items-center gap-2 text-xs ${textMutedOnCanvas}`}>
              <span>{labels[row.cause]}</span>
              <AgentName actor={row.author} />
              <Timestamp at={row.created_at} />
              {row.snoozed_until !== null &&
              compareTimestamps(row.snoozed_until, new Date().toISOString()) > 0 ? (
                <span>Later</span>
              ) : null}
            </div>
            <MarkdownPreview className="mt-2 text-sm" fullTitle lines={3} markdown={row.body} />
            {row.cause === "thread_reply" || row.cause === "direct_reply" ? (
              <MarkdownPreview
                className={`mt-1 text-xs ${textMutedOnCanvas}`}
                fullTitle
                lead="In "
                lines={1}
                markdown={row.subject}
              />
            ) : null}
            <Link
              className={`mt-2 inline-block min-h-11 py-2 text-sm ${linkText} ${linkHoverText}`}
              to={row.href}
            >
              Open
            </Link>
          </li>
        ))}
      </ul>
      {inbox.hasNextPage ? (
        <button
          className={`mt-4 min-h-11 rounded-lg border px-3 py-2 text-sm ${borderDefault} ${linkText}`}
          disabled={inbox.isFetchingNextPage}
          onClick={() => void inbox.fetchNextPage()}
          type="button"
        >
          {inbox.isFetchingNextPage ? "Loading…" : "Load older"}
        </button>
      ) : null}
    </section>
  );
}

import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { myRepliesKey } from "../../api/queries";
import type { MyRepliesResponse, MyReplyRow } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { QueryError } from "../../components/QueryError";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { AskOwnerLink } from "../inbox/AskOwnerLink";
import { actorLabel } from "../refs/actor";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

const PAGE_SIZE = 50;

/** The page's own key beneath the prefix every Replies query shares, so a mark anywhere, and every
 *  event that can move a row (`api/sse.ts`), refreshes every loaded page together. */
const PAGE_KEY = [...myRepliesKey, "page"] as const;

function ReplyRow({ row }: { row: MyReplyRow }): ReactNode {
  const queryClient = useQueryClient();
  const markRead = useMutation({
    mutationFn: () => api.markThreadRead(row.thread_id, row.reply.created_at),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: myRepliesKey }),
  });
  const owner =
    row.issue !== undefined
      ? { issue: row.issue }
      : row.document !== undefined
        ? { document: row.document }
        : undefined;
  return (
    <li
      className={`rounded-xl border p-3 ${card} ${borderDefault}`}
      data-replies-row={row.thread_id}
    >
      <div className={`flex flex-wrap items-center gap-x-2 gap-y-1 text-xs ${textMutedOnCanvas}`}>
        <span className={`font-semibold ${textPrimaryOnCanvas}`}>
          {actorLabel(row.reply.author)}
        </span>
        {/* An answer someone else gave is the thread's newest write; its first line is the chosen
            options and the answer's text. */}
        {row.reply.kind === "answer" ? <span>answered</span> : null}
        <Timestamp at={row.reply.created_at} />
        {owner === undefined ? null : <AskOwnerLink owner={owner} />}
      </div>
      <MarkdownPreview
        className={`mt-1 text-sm ${textPrimaryOnCanvas}`}
        fullTitle
        lines={2}
        markdown={row.reply.first_line}
      />
      <MarkdownPreview
        className={`mt-1 text-xs ${textMutedOnCanvas}`}
        fullTitle
        lead="In "
        lines={1}
        markdown={row.subject}
      />
      <div className="mt-2 flex flex-wrap items-center gap-4 text-sm">
        {/* The server's `ref` names the thread's root, never the reply: an anchored root redirects
            to its document, and a comment's thread opens as it lands. */}
        <Link className={`font-medium ${linkText} ${linkHoverText}`} to={row.ref}>
          Open
        </Link>
        <button
          className={`min-h-11 font-medium ${linkText} ${linkHoverText}`}
          disabled={markRead.isPending}
          onClick={() => markRead.mutate()}
          type="button"
        >
          {markRead.isPending ? "Marking read…" : "Mark read"}
        </button>
      </div>
      {markRead.isError ? (
        <div className="mt-2">
          <QueryError message={markRead.error.message} onRetry={() => markRead.mutate()} />
        </div>
      ) : null}
    </li>
  );
}

/** `/replies`: every thread the signed-in person wrote in - an ask's thread, a comment thread, an
 *  issue message thread - where someone else has written since they last read it, newest reply
 *  first, from `GET /api/v1/me/replies`. Reading the thread, replying in it, or Mark read clears
 *  its row; a later reply brings it back. */
export function RepliesPage(): ReactNode {
  useDocumentTitle("Replies · Dispatch");
  const replies = useInfiniteQuery({
    getNextPageParam: (last: MyRepliesResponse) =>
      last.offset + last.rows.length < last.total ? last.offset + last.rows.length : undefined,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.listMyReplies({ limit: PAGE_SIZE, offset: pageParam }),
    queryKey: PAGE_KEY,
  });

  if (replies.isPending) return <LoadingSkeleton label="Loading your replies" />;
  if (replies.isError) {
    return <p className={dangerText}>Could not load your replies: {replies.error.message}</p>;
  }
  // Pages are offsets into a list that moves as replies land, so a page fetched after a reply moved
  // a thread up can repeat a row an earlier page holds; the event that moved it refreshes every
  // loaded page from the top, and until then the thread shows once, where it was first listed.
  const seen = new Set<string>();
  const rows = replies.data.pages
    .flatMap((page) => page.rows)
    .filter((row) => !seen.has(row.thread_id) && seen.add(row.thread_id));

  return (
    <section aria-label="Replies">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <h1 className={`text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Replies
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Threads you wrote in where someone else has written since you last read them, newest
          first.
        </p>
      </header>
      {rows.length === 0 ? (
        <EmptyState label="Replies empty state" message="No new replies in threads you wrote in." />
      ) : (
        <ul className="space-y-3">
          {rows.map((row) => (
            <ReplyRow key={row.thread_id} row={row} />
          ))}
        </ul>
      )}
      {replies.hasNextPage ? (
        <button
          className={`mt-4 min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${linkText} ${linkHoverText}`}
          disabled={replies.isFetchingNextPage}
          onClick={() => void replies.fetchNextPage()}
          type="button"
        >
          {replies.isFetchingNextPage ? "Loading…" : "Show more"}
        </button>
      ) : null}
    </section>
  );
}

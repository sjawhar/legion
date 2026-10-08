import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { MyAnswerRow, MyAnswersResponse } from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { TruncatedText } from "../../components/TruncatedText";
import {
  borderDefault,
  card,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { AskCard } from "../inbox/AskCard";
import { MarkdownBody } from "../refs/MarkdownBody";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { referenceTriggerProps } from "../refs/RefPreview";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

const PAGE_SIZE = 50;

/** The issue or project document an answered ask belongs to, as the Inbox row names it. */
function OwnerLink({ row }: { row: MyAnswerRow }): ReactNode {
  if ("issue" in row.owner) {
    const { key, title } = row.owner.issue;
    return (
      <Link
        className={`flex min-w-0 items-baseline gap-2 text-sm ${linkText} ${linkHoverText}`}
        to={buildIssuePath({ key, kind: "issue" })}
        {...referenceTriggerProps({ key, kind: "issue" })}
      >
        <span className="shrink-0 font-semibold">{key}</span>
        <TruncatedText>{title}</TruncatedText>
      </Link>
    );
  }
  const { name, project, slug } = row.owner.document;
  return (
    <Link
      className={`min-w-0 truncate text-sm font-semibold ${linkText} ${linkHoverText}`}
      to={buildProjectPath({ kind: "document", project, slug })}
      {...referenceTriggerProps({ kind: "document", project, slug })}
    >
      <TruncatedText>
        {project} · {name}
      </TruncatedText>
    </Link>
  );
}

/** The ask a row's Change answer opens, read through the same `["ask-thread", id]` key the
 *  card's own thread query uses, so one fetch serves both; mounted in `change` mode, the card
 *  opens its form seeded with the current answer. */
function ChangeAnswerCard({ askId }: { askId: string }): ReactNode {
  const thread = useQuery({
    queryFn: () => api.getAsk(askId),
    queryKey: ["ask-thread", askId],
    staleTime: Number.POSITIVE_INFINITY,
  });
  if (thread.isPending) return <LoadingSkeleton label="Loading the ask" />;
  if (thread.isError) {
    return (
      <p className={`text-sm ${dangerText}`}>Could not load the ask: {thread.error.message}</p>
    );
  }
  return <AskCard ask={thread.data.ask} initialThread={thread.data} mode="change" />;
}

function AnswerRow({
  changing,
  onChange,
  row,
}: {
  changing: boolean;
  onChange: () => void;
  row: MyAnswerRow;
}): ReactNode {
  const changeable =
    row.kind === "answer" &&
    row.current &&
    row.ask_state === "answered" &&
    row.ask_kind !== "approval";
  return (
    <li className={`rounded-xl border p-3 ${card} ${borderDefault}`} data-answer-row={row.ask_id}>
      <div className={`flex flex-wrap items-center gap-x-2 gap-y-1 text-xs ${textMutedOnCanvas}`}>
        <Timestamp at={row.at} />
        <OwnerLink row={row} />
        {row.kind === "answer" && !row.current ? <LabelPill>Changed since</LabelPill> : null}
      </div>
      <MarkdownPreview
        className={`mt-1 text-sm font-medium ${textPrimaryOnCanvas}`}
        fullTitle
        lines={2}
        markdown={row.question}
      />
      {row.kind === "answer" && row.answer !== undefined ? (
        <div className={`mt-2 text-sm ${textPrimaryOnCanvas}`}>
          {row.answer.selected.length === 0 ? null : (
            <div className="flex flex-wrap gap-1.5">
              {row.answer.selected.map((label) => (
                <LabelPill key={label} selected>
                  {label}
                </LabelPill>
              ))}
            </div>
          )}
          {row.answer.text === null || row.answer.text === "" ? null : (
            <div className="mt-1">
              <MarkdownBody markdown={row.answer.text} />
            </div>
          )}
        </div>
      ) : null}
      {row.kind === "reply" && row.reply !== undefined ? (
        <p className={`mt-2 text-sm ${textPrimaryOnCanvas}`}>
          <span className={`font-medium ${textMutedOnCanvas}`}>Replied:</span>{" "}
          <MarkdownBody markdown={row.reply.body} variant="inline" />
        </p>
      ) : null}
      <div className="mt-2 flex flex-wrap items-center gap-4 text-sm">
        {/* The server's `ref` is the ask's own item route: an issue ask's lands an anchored ask on
            its document with the card selected and an unanchored one on its Conversation turn. */}
        <Link className={`font-medium ${linkText} ${linkHoverText}`} to={row.ref}>
          Open
        </Link>
        {changeable && !changing ? (
          <button
            className={`min-h-11 font-medium ${linkText} ${linkHoverText}`}
            onClick={onChange}
            type="button"
          >
            Change answer
          </button>
        ) : null}
      </div>
    </li>
  );
}

/** `/answers`: every answer the signed-in person gave and every reply they wrote on an ask,
 *  newest first, from one `GET /api/v1/me/answers`. A current, non-approval answer can be
 *  changed in place: the card opens below its row and stays open across the refetch the change
 *  causes, which puts the new answer at the top and marks the old row "Changed since". */
export function AnswersPage(): ReactNode {
  useDocumentTitle("Answered by you · Dispatch");
  const [changingAsk, setChangingAsk] = useState<string | null>(null);
  const answers = useInfiniteQuery({
    getNextPageParam: (last: MyAnswersResponse) =>
      last.offset + last.rows.length < last.total ? last.offset + last.rows.length : undefined,
    initialPageParam: 0,
    queryFn: ({ pageParam }) => api.listMyAnswers({ limit: PAGE_SIZE, offset: pageParam }),
    queryKey: ["me", "answers"],
  });

  if (answers.isPending) return <LoadingSkeleton label="Loading your answers" />;
  if (answers.isError) {
    return <p className={dangerText}>Could not load your answers: {answers.error.message}</p>;
  }
  const rows = answers.data.pages.flatMap((page) => page.rows);
  // The card belongs to the ask, under the first answer row that names it: after a change that is
  // the new answer's row, so the card the person is looking at stays mounted where they are. A
  // reply the person wrote on the same ask can sort above it, and is never the card's row.
  const cardRow =
    changingAsk === null
      ? -1
      : rows.findIndex((row) => row.kind === "answer" && row.ask_id === changingAsk);

  return (
    <section aria-label="Answered by you">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/">
          ← Inbox
        </Link>
        <h1 className={`mt-1 text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Answered by you
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          Every answer you gave and every reply you wrote on an ask, newest first.
        </p>
      </header>
      {rows.length === 0 ? (
        <EmptyState
          label="Answers empty state"
          message="You have not answered or replied on an ask yet."
        />
      ) : (
        // One flat list of keyed items, the card among them: a card nested under its row would
        // remount when the change puts a new row above it, and reopen its form.
        <ul className="space-y-3">
          {rows.flatMap((row, index) => {
            const item = (
              <AnswerRow
                changing={index === cardRow}
                key={
                  row.kind === "reply" ? `reply:${row.reply?.id}` : `answer:${row.ask_id}:${row.at}`
                }
                onChange={() => setChangingAsk(row.ask_id)}
                row={row}
              />
            );
            return index === cardRow
              ? [
                  item,
                  <li data-answer-card={row.ask_id} key={`card:${row.ask_id}`}>
                    <ChangeAnswerCard askId={row.ask_id} />
                  </li>,
                ]
              : [item];
          })}
        </ul>
      )}
      {answers.hasNextPage ? (
        <button
          className={`mt-4 min-h-11 rounded-lg border px-3 py-2 text-sm font-medium ${borderDefault} ${linkText} ${linkHoverText}`}
          disabled={answers.isFetchingNextPage}
          onClick={() => void answers.fetchNextPage()}
          type="button"
        >
          {answers.isFetchingNextPage ? "Loading…" : "Show more"}
        </button>
      ) : null}
    </section>
  );
}

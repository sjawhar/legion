import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Ask } from "../../api/types";
import {
  badgeLow,
  borderStrong,
  dangerText,
  linkHoverText,
  linkText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { actorLabel } from "../refs/actor";
import { buildIssuePath, buildProjectPath } from "../refs/routes";
import { AskCard } from "./AskCard";
import { BlockedOnYou, waitingOnYou } from "./BlockedOnYou";

function ReplyChip({ children }: { children: ReactNode }): ReactNode {
  return (
    <span
      className={`inline-block rounded-full px-2 py-0.5 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
    >
      {children}
    </span>
  );
}

/** A human spoke last on this ask (last_reply.author.kind === "user") — the ask's own author
 *  (an agent) owes the next turn, so the viewer is told who they're waiting on. Otherwise, if
 *  an agent replied last on a row still needing the viewer, that reply is surfaced so the
 *  viewer knows to look before answering. */
function InboxRowChip({ ask }: { ask: Ask }): ReactNode {
  if (ask.last_reply?.author.kind === "user") {
    return <ReplyChip>Waiting on {actorLabel(ask.author)}</ReplyChip>;
  }
  if (ask.last_reply?.author.kind === "session") {
    return <ReplyChip>{actorLabel(ask.last_reply.author)} replied</ReplyChip>;
  }
  return null;
}

function InboxRow({ ask }: { ask: Ask }): ReactNode {
  const owner = ask.issue?.key ?? ask.issue_key;
  const title = ask.issue?.title ?? owner ?? "Unassigned ask";
  return (
    <li>
      <div className="mb-2 flex flex-wrap items-center gap-2">
        {ask.document === undefined ? (
          owner === null ? (
            <p className={`text-sm ${textMutedOnCanvas}`}>{title}</p>
          ) : (
            <Link
              className={`flex flex-col items-start gap-1 text-sm md:inline-flex md:flex-row md:items-baseline md:gap-2 ${linkText} ${linkHoverText}`}
              to={buildIssuePath({ id: ask.id, key: owner, kind: "ask" })}
            >
              <span className="font-semibold">{owner}</span>
              <span>{title}</span>
            </Link>
          )
        ) : (
          <Link
            className={`flex flex-col items-start gap-1 text-sm font-semibold md:inline-flex md:flex-row md:items-baseline md:gap-2 ${linkText} ${linkHoverText}`}
            to={buildProjectPath({
              item: { id: ask.id, kind: "ask" },
              kind: "document",
              project: ask.document.project,
              slug: ask.document.slug,
            })}
          >
            {ask.document.project} · {ask.document.name}
          </Link>
        )}
        <InboxRowChip ask={ask} />
      </div>
      <AskCard ask={ask} />
    </li>
  );
}

export function Inbox(): ReactNode {
  const inbox = useQuery({
    queryKey: ["inbox"],
    queryFn: () => api.getInbox(),
  });

  if (inbox.isPending) {
    return <p className={textMutedOnCanvas}>Loading your inbox…</p>;
  }
  if (inbox.isError) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }
  if (inbox.data.length === 0) {
    return (
      <p className={`rounded-xl border border-dashed p-8 ${borderStrong} ${textMutedOnCanvas}`}>
        Nothing needs you
      </p>
    );
  }

  const waiting = waitingOnYou(inbox.data);
  const waitingIDs = new Set(waiting.map((ask) => ask.id));
  const remaining = inbox.data.filter((ask) => !waitingIDs.has(ask.id));
  const waitingOnAgents = remaining.filter((ask) => ask.last_reply?.author.kind === "user");
  const needsYou = remaining.filter((ask) => ask.last_reply?.author.kind !== "user");

  return (
    <div className="space-y-6">
      <BlockedOnYou asks={inbox.data} />
      {waiting.length === 0 ? null : (
        <section>
          <h2 className={`mb-3 text-sm font-semibold ${textMutedOnCanvas}`}>Waiting on you</h2>
          <ul className="space-y-3">
            {waiting.map((ask) => (
              <InboxRow ask={ask} key={ask.id} />
            ))}
          </ul>
        </section>
      )}
      {needsYou.length === 0 ? null : (
        <section>
          <h2 className={`mb-3 text-sm font-semibold ${textMutedOnCanvas}`}>Needs you</h2>
          <ul className="space-y-3">
            {needsYou.map((ask) => (
              <InboxRow ask={ask} key={ask.id} />
            ))}
          </ul>
        </section>
      )}
      {waitingOnAgents.length === 0 ? null : (
        <section>
          <h2 className={`mb-3 text-sm font-semibold ${textMutedOnCanvas}`}>Waiting on agents</h2>
          <ul className="space-y-3">
            {waitingOnAgents.map((ask) => (
              <InboxRow ask={ask} key={ask.id} />
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

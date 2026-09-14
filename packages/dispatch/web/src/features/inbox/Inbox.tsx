import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";
import { PriorityBadge } from "../../components/Badge";
import { EmptyState } from "../../components/EmptyState";
import { LoadingSkeleton } from "../../components/LoadingSkeleton";
import { LabelPill } from "../../components/Pill";
import { dangerText, linkHoverText, linkText, textMutedOnCanvas } from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { actorLabel } from "../refs/actor";
import { buildInboxPath, buildIssuePath, buildProjectPath, parseInboxSearch } from "../refs/routes";
import { AskCard } from "./AskCard";
import { BlockedOnYou, waitingOnYou } from "./BlockedOnYou";

function ReplyChip({ children }: { children: ReactNode }): ReactNode {
  return <LabelPill>{children}</LabelPill>;
}

/** A human spoke last on this ask (last_reply.author.kind === "user") — the ask's own author
 *  (an agent) owes the next turn, so the viewer is told who they're waiting on. Otherwise, if
 *  an agent replied last on a row still needing the viewer, that reply is surfaced so the
 *  viewer knows to look before answering. */
function InboxRowChip({ ask }: { ask: InboxRow }): ReactNode {
  if (ask.last_reply?.author.kind === "user") {
    return <ReplyChip>Waiting on {actorLabel(ask.author)}</ReplyChip>;
  }
  if (ask.last_reply?.author.kind === "session") {
    return <ReplyChip>{actorLabel(ask.last_reply.author)} replied</ReplyChip>;
  }
  return null;
}

function InboxItem({ ask }: { ask: InboxRow }): ReactNode {
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
        <PriorityBadge priority={ask.priority} />
      </div>
      <AskCard ask={ask} />
    </li>
  );
}

function AskSection({ asks, title }: { asks: readonly InboxRow[]; title: string }): ReactNode {
  if (asks.length === 0) return null;
  return (
    <section>
      <h2 className={`mb-3 text-base font-semibold ${textMutedOnCanvas}`}>{title}</h2>
      <ul className="space-y-3">
        {asks.map((ask) => (
          <InboxItem ask={ask} key={ask.id} />
        ))}
      </ul>
    </section>
  );
}

export function Inbox(): ReactNode {
  const { search } = useLocation();
  const filter = parseInboxSearch(search);
  const inbox = useQuery({
    queryKey: ["inbox"],
    queryFn: () => api.getInbox(),
  });
  const { titles } = useAgents(filter.agent !== undefined);

  if (inbox.isPending) {
    return <LoadingSkeleton label="Loading your inbox" />;
  }
  if (inbox.isError) {
    return <p className={dangerText}>Could not load your inbox.</p>;
  }

  const agent = filter.agent;
  const fromAgent =
    agent === undefined
      ? inbox.data
      : inbox.data.filter((ask) => ask.author.kind === "session" && ask.author.id === agent);
  const shown = filter.section === "needs-you" ? waitingOnYou(fromAgent) : fromAgent;
  // The live agent's title when Envoy still lists it; otherwise the author label its asks carry.
  const liveTitle = agent === undefined ? undefined : titles.get(agent)?.trim();
  const agentTitle =
    liveTitle !== undefined && liveTitle !== ""
      ? liveTitle
      : fromAgent[0] === undefined
        ? agent
        : actorLabel(fromAgent[0].author);
  const chip =
    agent === undefined ? null : (
      <Link
        aria-label="Clear agent filter"
        className="inline-flex min-h-11 items-center rounded-full"
        to={buildInboxPath()}
      >
        <LabelPill selected>
          Asks from {agentTitle}
          {filter.section === "needs-you" ? " waiting on you" : ""} · clear
        </LabelPill>
      </Link>
    );

  if (shown.length === 0) {
    return (
      <div className="space-y-6">
        {chip}
        <EmptyState
          label="Inbox empty state"
          message={agent === undefined ? "Nothing needs you" : `No open asks from ${agentTitle}`}
        />
      </div>
    );
  }

  const waiting = waitingOnYou(shown);
  const waitingOnAgents = shown.filter((ask) => ask.last_reply?.author.kind === "user");

  return (
    <div className="space-y-6">
      {chip}
      {agent === undefined ? <BlockedOnYou asks={inbox.data} /> : null}
      <AskSection asks={waiting} title="Waiting on you" />
      <AskSection asks={waitingOnAgents} title="Waiting on agents" />
    </div>
  );
}

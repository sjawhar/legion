import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
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
  textPrimaryOnCanvas,
} from "../../theme/classes";
import { resolveAuthor } from "../conversation/authors";
import { firstLine } from "../conversation/ReplyQuote";
import { useAgents } from "../conversation/useAgents";
import { Timestamp } from "../refs/Timestamp";
import { useDocumentTitle } from "../shell/useDocumentTitle";

/** Every broadcast this Dispatch has sent, newest first, so one sent minutes ago is reachable
 *  again after the sender navigated away from it. */
export function BroadcastsPage(): ReactNode {
  useDocumentTitle("Broadcasts · Dispatch");
  const { titles } = useAgents(true);
  const broadcasts = useQuery({
    queryFn: () => api.listBroadcasts(),
    queryKey: ["broadcast", "list"],
  });

  if (broadcasts.isPending) return <LoadingSkeleton label="Loading broadcasts" />;
  if (broadcasts.isError) {
    return (
      <p className={dangerText}>
        Could not load broadcasts:{" "}
        {broadcasts.error instanceof Error ? broadcasts.error.message : "network error"}
      </p>
    );
  }

  return (
    <section aria-label="Broadcasts">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/agents">
          ← Agents
        </Link>
        <h1 className={`mt-1 text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Broadcasts
        </h1>
        <p className={`mt-1 text-sm ${textMutedOnCanvas}`}>
          One message sent to many agents, and how each of them answered.
        </p>
      </header>
      {broadcasts.data.length === 0 ? (
        <EmptyState
          label="Broadcasts empty state"
          message="No broadcast has been sent from Dispatch yet."
        />
      ) : (
        <ul className="space-y-3">
          {broadcasts.data.map((sent) => (
            <li className={`rounded-xl border ${card} ${borderDefault}`} key={sent.id}>
              <Link className="block p-3" to={`/agents/broadcasts/${sent.id}`}>
                <p className={`flex flex-wrap items-center gap-2 text-xs ${textMutedOnCanvas}`}>
                  <LabelPill>{sent.delivery}</LabelPill>
                  <span>{resolveAuthor(sent.author, titles).label}</span>
                  <Timestamp at={sent.created_at} />
                  <span>
                    {sent.replies} of {sent.recipients} answered
                  </span>
                </p>
                <p className={`mt-1 truncate text-sm ${textPrimaryOnCanvas}`}>
                  {firstLine(sent.body)}
                </p>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

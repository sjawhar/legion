import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams } from "react-router-dom";

import { api } from "../../api/client";
import type { BroadcastRecipient } from "../../api/types";
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
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { resolveAuthor } from "../conversation/authors";
import { DeliveryStatus } from "../conversation/TargetedMessageCard";
import { useAgents } from "../conversation/useAgents";
import { sessionLabel } from "../refs/actor";
import { MarkdownBody } from "../refs/MarkdownBody";
import { Timestamp } from "../refs/Timestamp";

import { deliveryAttempts } from "./attempts";

/** How many recipients have answered: a recipient whose thread carries a message the session
 *  itself wrote. The human's own follow-ups in that thread are not answers. */
export function answeredCount(recipients: readonly BroadcastRecipient[]): number {
  return recipients.filter((recipient) =>
    recipient.replies.some((reply) => reply.author.kind === "session")
  ).length;
}

function BroadcastRecipientRow({
  recipient,
  titles,
}: {
  recipient: BroadcastRecipient;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const label = sessionLabel(recipient.session_id, titles.get(recipient.session_id) ?? "");
  const answer = recipient.replies.find((reply) => reply.author.kind === "session");
  return (
    <article aria-label={label} className={`rounded-xl border p-3 ${card} ${borderDefault}`}>
      <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>{label}</h3>
      {recipient.send_error === undefined || recipient.send_error === "" ? (
        <DeliveryStatus
          answeredBy={answer === undefined ? undefined : resolveAuthor(answer.author, titles).label}
          deliveries={deliveryAttempts(recipient.message.deliveries, label)}
          targetName={label}
        />
      ) : (
        <p className={`mt-2 text-sm font-semibold ${dangerText}`}>
          Not delivered: {recipient.send_error}
        </p>
      )}
      {recipient.replies.map((reply) => (
        <div className={`mt-2 border-t pt-2 ${borderDefault}`} key={reply.id}>
          <p className={`flex items-baseline gap-2 text-xs ${textSecondaryOnCanvas}`}>
            <span className="font-semibold">{resolveAuthor(reply.author, titles).label}</span>
            <Timestamp at={reply.created_at} />
          </p>
          <div className={textPrimaryOnCanvas}>
            <MarkdownBody markdown={reply.body} variant="inline" />
          </div>
        </div>
      ))}
    </article>
  );
}

/** One broadcast: what was sent, and where every recipient's copy of it got to. */
export function BroadcastPage(): ReactNode {
  const { id = "" } = useParams<{ id: string }>();
  const { titles } = useAgents(true, true);
  const broadcast = useQuery({
    queryFn: () => api.getBroadcast(id),
    // Keyed under ["broadcast"], the prefix every issue-less message event invalidates, so a
    // recipient's delivery receipt or reply lands here without a poll.
    queryKey: ["broadcast", id],
  });

  if (broadcast.isPending) return <LoadingSkeleton label="Loading broadcast" />;
  if (broadcast.isError) {
    return (
      <p className={dangerText}>
        Could not load this broadcast:{" "}
        {broadcast.error instanceof Error ? broadcast.error.message : "network error"}
      </p>
    );
  }

  const sent = broadcast.data;
  const answered = answeredCount(sent.recipients);
  return (
    <section aria-label="Broadcast">
      <header className={`mb-5 border-b pb-4 ${borderDefault}`}>
        <Link className={`text-sm ${linkText} ${linkHoverText}`} to="/agents/broadcasts">
          ← All broadcasts
        </Link>
        <h1 className={`mt-1 text-[22px] font-semibold tracking-tight ${textPrimaryOnCanvas}`}>
          Broadcast to {sent.recipients.length} {sent.recipients.length === 1 ? "agent" : "agents"}
        </h1>
        <p className={`mt-1 flex flex-wrap items-center gap-2 text-sm ${textMutedOnCanvas}`}>
          <LabelPill>{sent.delivery}</LabelPill>
          <span>{resolveAuthor(sent.author, titles).label}</span>
          <Timestamp at={sent.created_at} />
          <span>
            {answered} of {sent.recipients.length} answered
          </span>
        </p>
        <div className={`mt-2 ${textPrimaryOnCanvas}`}>
          <MarkdownBody markdown={sent.body} variant="inline" />
        </div>
      </header>
      {sent.recipients.length === 0 ? (
        <EmptyState label="Broadcast empty state" message="This broadcast reached no agent." />
      ) : (
        <div className="space-y-3">
          {sent.recipients.map((recipient) => (
            <BroadcastRecipientRow
              key={recipient.session_id}
              recipient={recipient}
              titles={titles}
            />
          ))}
        </div>
      )}
    </section>
  );
}

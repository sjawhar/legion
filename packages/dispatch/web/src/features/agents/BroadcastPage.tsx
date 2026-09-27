import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useLocation, useParams } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Agent,
  BroadcastExclusion,
  BroadcastRecipient,
  MessageDeliveryMode,
} from "../../api/types";
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
import {
  capabilitiesForTarget,
  DeliveryRetry,
  DeliveryStatus,
  offersSafeRetry,
} from "../conversation/TargetedMessageCard";
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

/** The sessions the send left out, handed over by the composer when it navigated here.
 *  Exclusions are not stored - they are a fact about one send, not about the broadcast - so
 *  this is the only place they can be shown, and only to the human who sent it. */
export function exclusionsFromState(state: unknown): readonly BroadcastExclusion[] {
  if (typeof state !== "object" || state === null) return [];
  const excluded = Reflect.get(state, "excluded");
  return Array.isArray(excluded) ? (excluded as BroadcastExclusion[]) : [];
}

function BroadcastRecipientRow({
  delivery,
  liveAgents,
  recipient,
  titles,
}: {
  delivery: MessageDeliveryMode;
  liveAgents: readonly Agent[];
  recipient: BroadcastRecipient;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (mode: MessageDeliveryMode) =>
      api.createMessageDelivery(recipient.message.id, mode),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["broadcast"] }),
  });
  const label = sessionLabel(recipient.session_id, titles.get(recipient.session_id) ?? "");
  const answer = recipient.replies.find((reply) => reply.author.kind === "session");
  const answeredBy = answer === undefined ? undefined : resolveAuthor(answer.author, titles).label;
  const attempts = deliveryAttempts(recipient.message.deliveries, label);
  const latest = attempts.at(-1);
  // Only a delivered attempt is settled news. A recipient with no attempt was never sent to -
  // delivery runs behind the send, and a shutdown can cut it - and an attempt still pending is
  // a send whose outcome nobody learned. Both are retryable, in the attempt's own mode safely.
  const retryable = answeredBy === undefined && latest?.state !== "sent";
  const capabilities = capabilitiesForTarget(recipient.message.target, liveAgents);
  return (
    <article aria-label={label} className={`rounded-xl border p-3 ${card} ${borderDefault}`}>
      <h3 className={`text-sm font-semibold ${textPrimaryOnCanvas}`}>{label}</h3>
      {latest === undefined ? (
        <p className={`mt-2 text-sm font-semibold ${dangerText}`}>
          Not sent to {label}: no delivery attempt was recorded.
        </p>
      ) : (
        <DeliveryStatus
          answeredBy={answeredBy}
          deliveries={attempts}
          retryOffered={offersSafeRetry(attempts, retryable)}
          targetName={label}
        />
      )}
      {retryable ? (
        <DeliveryRetry
          canAside={capabilities?.includes("aside") !== false}
          canBtw={capabilities?.includes("btw") !== false}
          canSteer={capabilities?.includes("steer") !== false}
          mode={latest?.delivery ?? delivery}
          onRetry={retry.mutate}
          retrying={retry.isPending}
          sameModeRetry={offersSafeRetry(attempts, true)}
          targetName={label}
        />
      ) : null}
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
  const location = useLocation();
  const excluded = exclusionsFromState(location.state);
  const { agents, titles } = useAgents(true, true);
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
        {excluded.length === 0 ? null : (
          <p className={`mt-2 text-sm ${dangerText}`}>
            Excluded:{" "}
            {excluded
              .map((item) => `${sessionLabel(item.session_id, item.title)} (${item.reason})`)
              .join(", ")}
            . Nothing was sent to them.
          </p>
        )}
      </header>
      {sent.recipients.length === 0 ? (
        <EmptyState label="Broadcast empty state" message="This broadcast reached no agent." />
      ) : (
        <div className="space-y-3">
          {sent.recipients.map((recipient) => (
            <BroadcastRecipientRow
              delivery={sent.delivery}
              key={recipient.session_id}
              liveAgents={agents}
              recipient={recipient}
              titles={titles}
            />
          ))}
        </div>
      )}
    </section>
  );
}

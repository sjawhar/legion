import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import { whoAmIQuery } from "../../api/queries";
import type { Ask, AskFollower, Subscriber } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import { textMutedOnSurface } from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { UnsubscribeDialog } from "../issue/UnsubscribeDialog";
import { sessionLabel } from "../refs/actor";
import { LiveDot, sessionChipClassName, sessionRemoveButtonClassName } from "../refs/SessionChip";

const removeAskFollower = (askId: string, sessionId: string): Promise<void> =>
  api.removeAskFollower(askId, sessionId);

const EMPTY_SUBSCRIBERS: readonly Subscriber[] = [];

/** The ask's owner, whose topic subscribers also hear every event on the ask. An ask on an
 *  issue (including one in an issue-linked document) routes through the issue's topics; an
 *  ask on a project document routes through the document's. */
type AskOwner = Pick<Ask, "artifact_id" | "issue_key">;

/** The subscribers list shared with the owner's header: the issue's Subscribed agents panel
 *  (`IssueHeader`) or the project document page. Same key, so a removal from either surface
 *  refreshes both, and an SSE `subscription.removed` refreshes this card too. */
function ownerSubscription(owner: AskOwner):
  | {
      id: string;
      label: string;
      list: () => Promise<Subscriber[]>;
      noun: "issue" | "document";
      remove: (sessionId: string) => Promise<void>;
    }
  | undefined {
  if (owner.issue_key !== null) {
    const key = owner.issue_key;
    return {
      id: key,
      label: key,
      list: () => api.getIssueSubscribers(key),
      noun: "issue",
      remove: (sessionId) => api.unsubscribeIssueSession(key, sessionId),
    };
  }
  if (owner.artifact_id !== undefined && owner.artifact_id !== null) {
    const id = owner.artifact_id;
    return {
      id,
      label: "this document",
      list: () => api.getArtifactSubscribers(id),
      noun: "document",
      remove: (sessionId) => api.unsubscribeArtifactSession(id, sessionId),
    };
  }
  return undefined;
}

export interface AskRecipientsProps {
  askId: string;
  /** Oldest first, as `GET /api/v1/asks/{id}` returns them. */
  followers: readonly AskFollower[];
  owner: AskOwner;
  /** Write seam for tests; defaults to the real API. */
  removeFollower?: (askId: string, sessionId: string) => Promise<void>;
}

type Confirming =
  | { kind: "follower"; sessionId: string }
  | { kind: "subscriber"; sessionId: string };

/**
 * Every session the server routes this ask's answer and replies to, under `Reaches N`: the
 * ask's followers (every session that wrote to the ask, minus any that unfollowed), then
 * every session subscribed to the owner's topic family that is not already a follower,
 * under `via issue subscription` (or `via document subscription`). Each shows a live/offline
 * dot and its title (or shortened session id). A follower's Unfollow and a subscriber's
 * Unsubscribe each confirm first and then cut the route that actually delivers; a subscriber
 * whose only matching topic is wider than this owner (e.g. `notifications.dispatch.>`) names
 * that topic and has no control, as the header's Subscribed agents panel does. When the
 * subscribers fetch fails, the card says so with a retry instead of passing the follower count
 * off as the whole set.
 */
export function AskRecipients({
  askId,
  followers,
  owner,
  removeFollower = removeAskFollower,
}: AskRecipientsProps): ReactNode {
  const queryClient = useQueryClient();
  const subscription = ownerSubscription(owner);
  // The subscribers endpoints are human-only; the card only asks once the signed-in principal
  // is known to be a user, so no other bearer ever cycles on a 403.
  const whoAmI = useQuery(whoAmIQuery());
  // Same key as the issue header (`IssueHeader`) and the project document page
  // (`DocumentPage`), so an Inbox of cards on one issue shares one fetch with its header. Each
  // fetch is two Envoy listener round trips (interests + sessions), so it is held fresh for a
  // while rather than refetched on every mount and focus; SSE invalidation still refreshes it.
  const subscribersQuery = useQuery({
    enabled: subscription !== undefined && whoAmI.data?.kind === "user",
    queryKey: ["subscribers", subscription?.id],
    queryFn: () => {
      if (subscription === undefined) {
        throw new Error("Ask recipients query requires an owner.");
      }
      return subscription.list();
    },
    staleTime: 30_000,
  });
  const subscriberRows = subscribersQuery.data ?? EMPTY_SUBSCRIBERS;
  const subscribers = subscriberRows.filter(
    (subscriber) => !followers.some((row) => row.session_id === subscriber.session_id)
  );
  const { agents } = useAgents(followers.length > 0);
  const [confirming, setConfirming] = useState<Confirming | undefined>(undefined);
  const unfollow = useMutation({
    mutationFn: (sessionId: string) => removeFollower(askId, sessionId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["ask-thread", askId] }),
  });
  const unsubscribe = useMutation({
    mutationFn: (sessionId: string) => {
      if (subscription === undefined) {
        throw new Error("Ask recipients unsubscribe requires an owner.");
      }
      return subscription.remove(sessionId);
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["subscribers", subscription?.id] }),
  });

  if (followers.length === 0 && subscribers.length === 0 && !subscribersQuery.isError) {
    return null;
  }

  const followerLabel = (sessionId: string): string =>
    sessionLabel(sessionId, agents.find((agent) => agent.session_id === sessionId)?.title);
  const viaLabel = subscription === undefined ? "" : `via ${subscription.noun} subscription`;

  return (
    <section aria-label="Recipients" className="mt-2">
      <h3 className={`text-xs font-semibold ${textMutedOnSurface}`}>
        Reaches {followers.length + subscribers.length}
      </h3>
      {followers.length === 0 ? null : (
        <ul aria-label="Followers" className="mt-1 flex flex-wrap gap-2">
          {followers.map((follower) => {
            const live = agents.some((agent) => agent.session_id === follower.session_id);
            return (
              <li className={sessionChipClassName} key={follower.session_id}>
                <LiveDot live={live} />
                <span title={follower.session_id}>{followerLabel(follower.session_id)}</span>
                <button
                  className={sessionRemoveButtonClassName}
                  disabled={unfollow.isPending}
                  onClick={() =>
                    setConfirming({ kind: "follower", sessionId: follower.session_id })
                  }
                  type="button"
                >
                  Unfollow
                </button>
              </li>
            );
          })}
        </ul>
      )}
      {subscribers.length === 0 ? null : (
        <>
          <p className={`mt-1.5 text-xs italic ${textMutedOnSurface}`}>{viaLabel}</p>
          <ul
            aria-label={`Via ${subscription?.noun} subscription`}
            className="mt-1 flex flex-wrap gap-2"
          >
            {subscribers.map((subscriber) => (
              <li className={sessionChipClassName} key={subscriber.session_id}>
                <LiveDot live={subscriber.live} />
                <span title={subscriber.session_id}>
                  {sessionLabel(subscriber.session_id, subscriber.title)}
                </span>
                {subscriber.removable ? (
                  <button
                    className={sessionRemoveButtonClassName}
                    disabled={unsubscribe.isPending}
                    onClick={() =>
                      setConfirming({ kind: "subscriber", sessionId: subscriber.session_id })
                    }
                    type="button"
                  >
                    Unsubscribe
                  </button>
                ) : (
                  <span
                    className="text-xs italic"
                    title={`Subscribed only via ${subscriber.via}, which also covers other issues and documents — cannot unsubscribe just this one`}
                  >
                    via {subscriber.via}
                  </span>
                )}
              </li>
            ))}
          </ul>
        </>
      )}
      {subscribersQuery.isError ? (
        <div className="mt-2">
          <QueryError
            message="Subscribers unavailable — Envoy listener unreachable."
            onRetry={() => subscribersQuery.refetch()}
            retrying={subscribersQuery.isFetching}
          />
        </div>
      ) : null}
      {unfollow.isError ? (
        <div className="mt-2">
          <QueryError
            message="Could not unfollow."
            onRetry={() => unfollow.mutate(unfollow.variables)}
            retrying={unfollow.isPending}
          />
        </div>
      ) : null}
      {unsubscribe.isError ? (
        <div className="mt-2">
          <QueryError
            message="Could not unsubscribe."
            onRetry={() => unsubscribe.mutate(unsubscribe.variables)}
            retrying={unsubscribe.isPending}
          />
        </div>
      ) : null}
      {confirming === undefined ? null : confirming.kind === "follower" ? (
        // Unfollowing cuts the follower route only: a session also subscribed to the owner still
        // hears the ask, and the card then shows it under the subscription group.
        <UnsubscribeDialog
          label="Unfollow"
          message={`Unfollow ${followerLabel(confirming.sessionId)} from this ask? ${
            subscription !== undefined &&
            subscriberRows.some((row) => row.session_id === confirming.sessionId)
              ? `Its answer and replies still reach them through the ${subscription.noun} subscription.`
              : "Its answer and replies will no longer reach them."
          } They will be told.`}
          onCancel={() => setConfirming(undefined)}
          onConfirm={() => {
            unfollow.mutate(confirming.sessionId);
            setConfirming(undefined);
          }}
        />
      ) : (
        <UnsubscribeDialog
          message={`Unsubscribe ${sessionLabel(confirming.sessionId, subscribers.find((row) => row.session_id === confirming.sessionId)?.title)} from ${subscription?.label}? They will be told.`}
          onCancel={() => setConfirming(undefined)}
          onConfirm={() => {
            unsubscribe.mutate(confirming.sessionId);
            setConfirming(undefined);
          }}
        />
      )}
    </section>
  );
}

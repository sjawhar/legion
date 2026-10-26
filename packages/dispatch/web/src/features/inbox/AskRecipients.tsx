import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import type { Ask, AskFollower, IssueRouteReach, Subscriber } from "../../api/types";
import { DisclosureToggle } from "../../components/DisclosureToggle";
import { QueryError } from "../../components/QueryError";
import { useIsHuman } from "../../hooks/useIsHuman";
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
  /** The session holding the claim on the ask's issue, from the same thread read. */
  claimant: string | null;
  /** Oldest first, as `GET /api/v1/asks/{id}` returns them. */
  followers: readonly AskFollower[];
  owner: AskOwner;
  /** Write seam for tests; defaults to the real API. */
  removeFollower?: (askId: string, sessionId: string) => Promise<void>;
  /** The ask's issue's route (`session:<id>` or `role:<name>`), from the same thread read. */
  route: string | null;
  /** The live session the route reaches now (the routed session, or the role's holder), from the
   *  same thread read; null when nobody holds the route or the listener did not say. */
  routeHolder: string | null;
  /** Whether the route reaches anyone, from the same thread read: `unknown` when the Envoy
   *  listener did not answer, so a null `routeHolder` says nothing about who receives it. */
  routeStatus: IssueRouteReach["route_status"];
}

/** The issue page's marker for a route the listener could not judge (`IssueHeader`). */
const ROUTE_REACH_UNKNOWN = "Route reach unknown";
const ROUTE_REACH_UNKNOWN_TITLE =
  "The Envoy listener did not answer, so Dispatch cannot tell whether anyone receives this route.";

/** A row the ask reaches through its issue rather than through a follow or a subscription: the
 *  claimant (`via claim`) or the route (`via route`). Neither is removable from the card; the
 *  claim and the route change on the issue. */
export interface IssueReachRow {
  /** The session the row reaches, or the route itself (`role:<name>`) while no session is known
   *  to hold it. */
  id: string;
  /** Whether a session receives through the row now. A role route that no session holds does
   *  not: its row names the route, offline, and `Reaches N` leaves it out. */
  reaches: boolean;
  /** Set on the route's row while the listener did not answer (`route_status: unknown`): the row
   *  claims neither live nor offline. A `session:<id>` route still names and counts its session,
   *  which is published to either way; a role route names the bare route, uncounted, and the
   *  count says the route reaches an unknown number more (`reachLabel`). */
  reachUnknown?: true;
  /** `via claim`, `via route`, or `via route (role:<name>)` on the session holding a role route. */
  via: string;
}

/** Every row the card lists, each session once, and how many distinct sessions they reach. */
export interface Reach {
  /** Distinct sessions a person's answer and replies are known to reach now: the `Reaches N`
   *  count. */
  count: number;
  issueRows: IssueReachRow[];
  /** The session the route delivers to: its holder, else a `session:<id>` route's own session
   *  (offline or unjudged, since the listener does not list it). Undefined for no route or a role
   *  route with no known holder. */
  routeSession: string | undefined;
  /** The owner's subscribers that are not followers, in the order the subscribers read lists them. */
  subscribers: Subscriber[];
  /** A role route the listener could not judge: it may reach a session `count` leaves out. */
  unknownRoute: boolean;
}

/** The card's `Reaches N` toggle, `Reaches N + route (unknown)` while a role route's reach is
 *  unknown, so the count never reads as the route reaching nobody. */
export function reachLabel({ count, unknownRoute }: Pick<Reach, "count" | "unknownRoute">): string {
  return unknownRoute ? `Reaches ${count} + route (unknown)` : `Reaches ${count}`;
}

/**
 * Lists each receiving session once: as a follower, else as a subscriber (both keep their
 * control), else under `via claim`, else under `via route`. The route's receiving session goes
 * through the same dedupe as the claimant's, so a role holder that is also the claimant, a
 * follower or a subscriber is counted once. A role route no session holds is listed as the route
 * itself and counted nowhere, since it reaches nobody now. While the listener did not answer
 * (`routeStatus` `unknown`) the route's row is marked reach unknown: a role route is still the
 * bare route, uncounted, and flagged `unknownRoute` so the count does not read as reaching nobody;
 * a session route still counts its session, which is published to either way.
 */
export function reachRows({
  claimant,
  followers,
  route,
  routeHolder,
  routeStatus,
  subscribers: subscriberRows,
}: {
  claimant: string | null;
  followers: readonly AskFollower[];
  route: string | null;
  routeHolder: string | null;
  routeStatus: IssueRouteReach["route_status"];
  subscribers: readonly Subscriber[];
}): Reach {
  const listed = new Set(followers.map((follower) => follower.session_id));
  const subscribers = subscriberRows.filter((subscriber) => !listed.has(subscriber.session_id));
  for (const subscriber of subscribers) {
    listed.add(subscriber.session_id);
  }
  const issueRows: IssueReachRow[] = [];
  const unknown = route !== null && routeStatus === "unknown";
  const addIssueRow = (sessionId: string, via: string, reachUnknown = false): void => {
    if (listed.has(sessionId)) {
      return;
    }
    listed.add(sessionId);
    issueRows.push(
      reachUnknown
        ? { id: sessionId, reaches: true, reachUnknown, via }
        : { id: sessionId, reaches: true, via }
    );
  };
  if (claimant !== null) {
    addIssueRow(claimant, "via claim");
  }
  const role = route?.startsWith("role:") === true;
  const routeSession =
    routeHolder ?? (route?.startsWith("session:") ? route.slice("session:".length) : undefined);
  if (routeSession !== undefined) {
    addIssueRow(routeSession, role ? `via route (${route})` : "via route", unknown);
  } else if (route !== null && role) {
    issueRows.push(
      unknown
        ? { id: route, reaches: false, reachUnknown: true, via: "via route" }
        : { id: route, reaches: false, via: "via route" }
    );
  }
  return {
    count: listed.size,
    issueRows,
    routeSession,
    subscribers,
    unknownRoute: unknown && role && routeSession === undefined,
  };
}

type Confirming =
  | { kind: "follower"; sessionId: string }
  | { kind: "subscriber"; sessionId: string };

/**
 * Every session the server routes this ask's answer and replies to, under `Reaches N`: the
 * ask's followers (every session that wrote to the ask, minus any that unfollowed); then the
 * session holding the issue's claim, under `via claim`, and the session the issue's route reaches,
 * under `via route` (`via route (role:<name>)` for a role's holder; the bare `role:<name>`, offline
 * and uncounted, while nobody holds the role); then every session subscribed to the owner's topic
 * family, under `via issue subscription` (or `via document subscription`). Each session is listed
 * and counted once (`reachRows`): as a follower, else as a subscriber, else via the claim, else
 * via the route, so a session keeps the control it has. Each shows a live/offline dot and its
 * title (or shortened session id), except the route's row while the listener did not answer: it
 * shows the issue page's `Route reach unknown` marker instead of a dot, and a role route then
 * reads `Reaches N + route (unknown)` (`reachLabel`). A follower's Unfollow and a subscriber's
 * Unsubscribe each confirm first, name what still reaches the session (a subscription, the claim
 * or the route), and then cut the route that actually delivers; the claim and route rows have no
 * control, since both change on the issue. A subscriber whose only matching topic is
 * wider than this owner (e.g. `notifications.dispatch.>`) names that topic and has no control,
 * as the header's Subscribed agents panel does. When the subscribers fetch fails, the card says
 * so with a retry instead of passing the follower count off as the whole set.
 */
export function AskRecipients({
  askId,
  claimant,
  followers,
  owner,
  removeFollower = removeAskFollower,
  route,
  routeHolder,
  routeStatus,
}: AskRecipientsProps): ReactNode {
  const queryClient = useQueryClient();
  const subscription = ownerSubscription(owner);
  // The subscribers endpoints are human-only; the card only asks once the signed-in principal
  // is known to be a user, so no other bearer ever cycles on a 403.
  const human = useIsHuman();
  // Same key as the issue header (`IssueHeader`) and the project document page
  // (`DocumentPage`), so an Inbox of cards on one issue shares one fetch with its header. Each
  // fetch is two Envoy listener round trips (interests + sessions), so it is held fresh for a
  // while rather than refetched on every mount and focus; SSE invalidation still refreshes it.
  const subscribersQuery = useQuery({
    enabled: subscription !== undefined && human,
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
  const { agents } = useAgents(followers.length > 0 || claimant !== null || route !== null);
  const followerLabel = (sessionId: string): string =>
    sessionLabel(sessionId, agents.find((agent) => agent.session_id === sessionId)?.title);
  const { count, issueRows, routeSession, subscribers, unknownRoute } = reachRows({
    claimant,
    followers,
    route,
    routeHolder,
    routeStatus,
    subscribers: subscriberRows,
  });
  const [confirming, setConfirming] = useState<Confirming | undefined>(undefined);
  const [open, setOpen] = useState(false);
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

  if (
    followers.length === 0 &&
    issueRows.length === 0 &&
    subscribers.length === 0 &&
    !subscribersQuery.isError
  ) {
    return null;
  }

  const viaLabel = subscription === undefined ? "" : `via ${subscription.noun} subscription`;
  // What still delivers to a session once its claim or route is all that is left; the route's
  // session is a role's holder too.
  const issueReach = (sessionId: string): string | undefined => {
    if (sessionId === claimant) {
      return "A person's answer and replies still reach them through the claim.";
    }
    if (sessionId === routeSession) {
      return "A person's answer and replies still reach them through the route.";
    }
    return undefined;
  };
  // What still delivers to a session once it is unfollowed, named by the group it moves into.
  const stillReachesUnfollowed = (sessionId: string): string => {
    if (subscription !== undefined && subscriberRows.some((row) => row.session_id === sessionId)) {
      return `Its answer and replies still reach them through the ${subscription.noun} subscription.`;
    }
    return issueReach(sessionId) ?? "Its answer and replies will no longer reach them.";
  };

  return (
    <section aria-label="Recipients" className="mt-2">
      <DisclosureToggle
        expanded={open}
        label={reachLabel({ count, unknownRoute })}
        onToggle={() => setOpen((shown) => !shown)}
        textClassName={textMutedOnSurface}
      />
      {!open ? null : (
        <>
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
          {issueRows.length === 0 ? null : (
            <ul aria-label="Via claim or route" className="mt-1 flex flex-wrap gap-2">
              {issueRows.map((row) => (
                <li className={sessionChipClassName} key={`${row.via}:${row.id}`}>
                  {row.reachUnknown ? null : (
                    <LiveDot
                      live={row.reaches && agents.some((agent) => agent.session_id === row.id)}
                    />
                  )}
                  <span title={row.id}>{row.reaches ? followerLabel(row.id) : row.id}</span>
                  <span className="text-xs italic">{row.via}</span>
                  {row.reachUnknown ? (
                    <span
                      className={`text-xs ${textMutedOnSurface}`}
                      title={ROUTE_REACH_UNKNOWN_TITLE}
                    >
                      {ROUTE_REACH_UNKNOWN}
                    </span>
                  ) : null}
                </li>
              ))}
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
        // Unfollowing cuts the follower route only: a session that is also the claimant, the
        // route session, or subscribed to the owner still hears the ask, and the card then shows
        // it under that group.
        <UnsubscribeDialog
          label="Unfollow"
          message={`Unfollow ${followerLabel(confirming.sessionId)} from this ask? ${stillReachesUnfollowed(
            confirming.sessionId
          )} They will be told.`}
          onCancel={() => setConfirming(undefined)}
          onConfirm={() => {
            unfollow.mutate(confirming.sessionId);
            setConfirming(undefined);
          }}
        />
      ) : (
        // Unsubscribing cuts the subscription only: a claimant or the route session still hears a
        // person's answer and replies, and the card then shows it under that group.
        <UnsubscribeDialog
          message={[
            `Unsubscribe ${sessionLabel(confirming.sessionId, subscribers.find((row) => row.session_id === confirming.sessionId)?.title)} from ${subscription?.label}?`,
            issueReach(confirming.sessionId),
            "They will be told.",
          ]
            .filter((part) => part !== undefined)
            .join(" ")}
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

import {
  ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS,
  type IssueClaim,
  type IssueClaimHolderState,
  type IssueClaimLiveness,
} from "./dispatch-api";

/**
 * Whether an issue's claim still holds it, judged against the live agent registry. The dashboard's
 * claim chip and the agent tools' claim lines both judge a claim here, so a person and an agent
 * reading the same issue see the same answer.
 *
 * - `holds`: a person's claim, which never lapses (there is no session to end, and only a person
 *   releases or forces it), or a session's the listener calls `live`.
 * - `unresponsive`: a session's claim whose holder is past the listener's live window and inside
 *   its gone horizon. It has missed heartbeats, it has not ended, and it keeps the issue: under
 *   box load a working session drops out of its heartbeats, and handing its issue to another
 *   agent is not a mistake the next read undoes (dispatch://INFRA-392). The server refuses
 *   another agent's take until `takeable_at` (`claimTakeableAt`), which a surface showing this
 *   state names, so a reader knows whether to wait or to pick the issue up.
 * - `lapsed`: a session's claim whose holder is gone - the listener calls it `gone`, or the
 *   loaded registry does not list it at all - which is exactly when the server lets any agent
 *   take the issue.
 * - `unknown`: a session's claim with no registry to judge it against (`titles` undefined: not yet
 *   loaded, or it could not be read). Treating that as lapsed would mark every claim free whenever
 *   the listener is unreachable.
 */
export type ClaimHolding = "holds" | "unresponsive" | "lapsed" | "unknown";

/**
 * What the reader knows about the holder's own registry record: the `holder_state`, `last_seen`
 * and `takeable_at` an issue read carries beside its claim (`IssueClaimLiveness`), as far as the
 * reader has them. A reader with no such read - a list row, a server that predates the fields -
 * passes none and is judged by the registry's titles alone, which name every session the
 * listener still holds a record of, live or unresponsive.
 */
export type ClaimHolderLiveness = Partial<IssueClaimLiveness>;

const HOLDING_OF: Record<IssueClaimHolderState, ClaimHolding> = {
  gone: "lapsed",
  live: "holds",
  unknown: "unknown",
  unresponsive: "unresponsive",
};

/**
 * `titles` is the live registry's session titles by session id, or undefined without one;
 * `liveness` the holder's state as an issue read reported it, which wins over the titles because
 * the server resolved it from the holder's own record rather than from a list's membership.
 */
export function claimHolds(
  claim: IssueClaim,
  titles: ReadonlyMap<string, string> | undefined,
  liveness: ClaimHolderLiveness = {}
): ClaimHolding {
  if (claim.actor.kind !== "session") {
    return "holds";
  }
  const state = liveness.holder_state;
  if (state != null) {
    return HOLDING_OF[state];
  }
  if (titles === undefined) {
    return "unknown";
  }
  return titles.has(claim.actor.id) ? "holds" : "lapsed";
}

/**
 * When another agent may take or release an unresponsive holder's claim without a person's
 * force, as an RFC3339 timestamp, or null when nothing to wait for is known: the read's own
 * `takeable_at` where it gave one, else the holder's last heartbeat plus
 * `ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS`, which is the sum the server itself refuses by
 * (`internal/dispatch/api/issue_claim.go`), so a chip predicting the wait and the refusal a
 * caller gets name one moment. `lastSeen` is the last registration in epoch milliseconds, as the
 * agent registry reports it, for a reader holding a registry row rather than an issue read.
 */
export function claimTakeableAt(
  liveness: ClaimHolderLiveness,
  lastSeen: number | undefined = undefined
): string | null {
  if (liveness.takeable_at != null) {
    return liveness.takeable_at;
  }
  if (liveness.holder_state !== "unresponsive") {
    return null;
  }
  const seen = liveness.last_seen == null ? lastSeen : Date.parse(liveness.last_seen);
  if (seen === undefined || !Number.isFinite(seen) || seen <= 0) {
    return null;
  }
  return new Date(seen + ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS).toISOString();
}

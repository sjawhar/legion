import type { ReactNode } from "react";

import { AGENT_LIST_REFILLING } from "../../api/client";
import type { IssueDetails } from "../../api/types";
import { TruncatedText } from "../../components/TruncatedText";
import { useTooltips } from "../../components/tooltips";
import {
  focusVisibleRing,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedStrongBg,
  textMutedOnSurface,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { sessionOfAddress } from "../refs/routes";
import { ClaimChip } from "./ClaimChip";
import type { IssueField } from "./IssueFieldForms";
import { authorText, PROPERTY_LABELS, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import {
  RouteReachUnknownMarker,
  UnreachableRouteMarker,
  UnresponsiveRouteMarker,
} from "./RouteReach";
import type { IssueHeaderWrites } from "./useIssueHeaderWrites";

/**
 * Row four of the identity block: where a message on this issue goes and whether it reaches
 * anyone, always; and who holds the issue, since when, and how it is handed back, while a claim
 * is on it.
 *
 * The two are next to each other because a reader confuses them, and each says which it is in
 * words: the route is where messages go, the claim is who is working it.
 */
export function IssueWorkRow({
  isClosed,
  issue,
  onOpenField,
  writes,
}: {
  isClosed: boolean;
  issue: IssueDetails;
  onOpenField: (field: IssueField) => void;
  writes: IssueHeaderWrites;
}): ReactNode {
  const agentText = useAgentText();
  const tooltips = useTooltips();
  const { drafts, releaseClaim } = writes;
  // A `session:` route is an agent: its control names it as `AgentName` does, with the agent's
  // own live link beside it. A role route is its address.
  const routeSession = sessionOfAddress(drafts.route);
  const routeLabel = `Messages default to ${
    drafts.route === ""
      ? "no route"
      : routeSession === undefined
        ? drafts.route
        : agentText.sessionText(routeSession, undefined)
  }`;
  const routeControl = (name: ReactNode) => (
    <button
      aria-label={routeLabel}
      className={`inline-flex min-h-11 max-w-[22ch] shrink-0 items-center truncate rounded-full px-3 py-2 text-left text-sm outline-none focus-visible:ring-2 md:min-h-8 md:py-1 xl:px-2 ${surfaceMutedStrongBg} ${textSecondaryOnSurface} ${textSecondaryHoverToPrimary} ${focusVisibleRing}`}
      disabled={isClosed}
      onClick={() => onOpenField("route")}
      type="button"
      {...authorText}
    >
      <TruncatedText>{name}</TruncatedText>
    </button>
  );

  return (
    <div
      className="flex w-full min-w-0 flex-wrap items-center gap-x-2 gap-y-1"
      data-testid="issue-work-row"
    >
      <span className={`shrink-0 text-sm font-medium ${textSecondaryOnSurface}`} {...pageChrome}>
        {PROPERTY_LABELS.route}
      </span>
      {routeSession === undefined ? (
        routeControl(drafts.route === "" ? "No route" : drafts.route)
      ) : (
        <AgentName
          className="inline-flex shrink-0 items-center"
          control={routeControl}
          kind="control"
          sessionId={routeSession}
          storedTitle={undefined}
        />
      )}
      {/* Whether the route reaches anyone speaks for the saved route only, so a route being
          saved hides it until the next read judges it. */}
      {drafts.route === (issue.route ?? "") ? (
        <>
          <UnreachableRouteMarker issue={issue} showRoute />
          <UnresponsiveRouteMarker issue={issue} showRoute />
          {issue.route !== null && issue.route_status === "unknown" ? (
            issue.route_status_reason === "refilling" ? (
              // A listener refilling its agent list has not failed, and says so rather than
              // reading as a listener that did not answer. Its reason is a tooltip only where
              // the surface raises one; the issue page raises none (R21).
              <span
                className={`shrink-0 text-xs ${textMutedOnSurface}`}
                data-testid="issue-route-refilling"
                title={
                  tooltips
                    ? "The Envoy listener is refilling its agent list, so Dispatch cannot tell yet whether anyone receives this route."
                    : undefined
                }
              >
                Route reach unknown: {AGENT_LIST_REFILLING}
              </span>
            ) : (
              <RouteReachUnknownMarker className="shrink-0" testId="issue-route-unknown" />
            )
          ) : null}
        </>
      ) : null}
      {issue.claim === null ? null : (
        <>
          {/* No `Claim:` label: the chip itself reads "Claimed by <holder> · <age>", which
              says which of the two this row's halves it is. */}
          <ClaimChip claim={issue.claim} liveness={issue} />
          <button
            aria-label={`Release the claim on ${issue.key}`}
            className={`min-h-11 shrink-0 rounded-lg px-2 py-2 text-sm font-medium sm:px-3 md:min-h-8 md:py-1 xl:px-2 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
            disabled={releaseClaim.isPending}
            onClick={() => releaseClaim.mutate()}
            type="button"
            {...pageChrome}
          >
            {releaseClaim.isPending ? "Releasing…" : CONTROL_LABELS.release_claim}
          </button>
          {/* No help line under the row: the paragraph explaining Release cost the identity
              block a whole line before the tree (dispatch://INFRA-1196 R6). */}
        </>
      )}
    </div>
  );
}

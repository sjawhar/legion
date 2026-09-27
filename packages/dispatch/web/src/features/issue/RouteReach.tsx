import type { ReactNode } from "react";

import type { IssueRouteReach } from "../../api/types";
import { calloutWarningBg, calloutWarningBorder, calloutWarningText } from "../../theme/classes";

type RoutedIssue = IssueRouteReach & { readonly route: string | null };

/**
 * The warning an issue carries wherever it is shown - the issue header, a List row, a Board
 * card - when its route reaches nobody: `route_status` is `no_holder`, meaning a role nobody
 * running holds, or a session that is not running, at the moment of the read. The server resolves the status on every
 * issue read, so this names it and judges nothing. A route that reaches a live session, an
 * unrouted issue, and a route the listener could not judge (`unknown`) render nothing here, so
 * healthy routes stay quiet and a listener restart does not mark every card.
 *
 * `showRoute` names the route in the label, for the header, where the marker sits in the state
 * row rather than beside the route. A session id is long, so only the route ellipsizes and the
 * verdict stays whole, the rule `ClaimChip` follows for a holder's name.
 */
export function UnreachableRouteMarker({
  issue,
  showRoute = false,
}: {
  issue: RoutedIssue;
  showRoute?: boolean;
}): ReactNode {
  if (issue.route === null || issue.route_status !== "no_holder") {
    return null;
  }
  const role = issue.route.startsWith("role:");
  const explanation = `${role ? `No running session holds ${issue.route} right now` : `${issue.route} is not running right now`}, so this issue's messages reach nobody at the moment. A session that is restarting or moving between boxes drops out of the listener for minutes and comes back under the same id, so this is a vacancy only when it is still true on a read ten minutes later; then staff the role, re-route it to a live holder, or clear the route and assign it.`;
  return (
    <span
      className={`inline-flex min-w-0 max-w-full items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-semibold ${calloutWarningBorder} ${calloutWarningBg} ${calloutWarningText}`}
      data-testid="issue-route-unreachable"
      style={{ flexShrink: showRoute ? 1 : 0 }}
      title={explanation}
    >
      <span aria-hidden="true" className="shrink-0">
        ⚠
      </span>
      {showRoute ? <span className="min-w-0 truncate">{issue.route}:</span> : null}
      <span className="shrink-0">
        {role ? "Nobody holds it right now" : "Not running right now"}
      </span>
      <span className="sr-only">. {explanation}</span>
    </span>
  );
}

import type { ReactNode } from "react";

import type { IssueRouteReach } from "../../api/types";
import {
  calloutWarningBg,
  calloutWarningBorder,
  calloutWarningText,
  textMutedOnSurface,
} from "../../theme/classes";
import { AgentName, useAgentText } from "../refs/AgentName";
import { sessionOfAddress } from "../refs/routes";

type RoutedIssue = IssueRouteReach & { readonly route: string | null };

/** The `Route reach unknown` marker for a route the Envoy listener could not judge
 *  (`route_status: unknown`), shown in the issue header and on an ask's route row
 *  (`AskRecipients`), with a title that says why. It claims neither that the route reaches
 *  someone nor that nobody holds it; the caller decides when to show it. `className` adds the
 *  call site's layout classes and `testId` its test id: the header's marker carries
 *  `issue-route-unknown` and an ask card's none, since the issue page shows both. */
export function RouteReachUnknownMarker({
  className,
  testId,
}: {
  className?: string;
  testId?: string;
}): ReactNode {
  return (
    <span
      className={`${className === undefined ? "" : `${className} `}text-xs ${textMutedOnSurface}`}
      data-testid={testId}
      title="The Envoy listener did not answer, so Dispatch cannot tell whether anyone receives this route."
    >
      Route reach unknown
    </span>
  );
}

/**
 * The warning an issue carries wherever it is shown - the issue header, a List row, a Board
 * card - when its route reaches nobody: `route_status` is `no_holder`, meaning a role nobody
 * running holds, or a session that is not running, as of the server's copy of the listener's
 * session list, at most 5 s old. The server resolves the status on every issue read, so this
 * names it and judges nothing. A route that reaches a live session, an
 * unrouted issue, and a route the listener could not judge (`unknown`) render nothing here, so
 * healthy routes stay quiet and a listener restart does not mark every card.
 *
 * `showRoute` names the route in the label, for the header, where the marker sits in the state
 * row rather than beside the route: a role as its address, a session by `AgentName`. A name can
 * be long, so only the route ellipsizes and the verdict stays whole, the rule `ClaimChip`
 * follows for a holder's name.
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
  return <UnreachableRoute route={issue.route} showRoute={showRoute} />;
}

/**
 * The marker for a route whose session - the routed session, or the role's holder - has missed
 * its heartbeats without ending (`route_status: holder_unresponsive`). It is neither of the
 * other two: the route does reach that session, because the listener stores what is sent while
 * it is silent, so this is not the warning `UnreachableRouteMarker` paints, and it is not the
 * `unknown` of a listener that could not judge the route either - the listener judged it and
 * said the session is stalled. It therefore shares the warning chip's shape, to sit in the same
 * row, in the muted tone a state that needs no action wears (dispatch://INFRA-392).
 */
export function UnresponsiveRouteMarker({
  issue,
  showRoute = false,
}: {
  issue: RoutedIssue;
  showRoute?: boolean;
}): ReactNode {
  const text = useAgentText();
  if (issue.route === null || issue.route_status !== "holder_unresponsive") {
    return null;
  }
  const holder = issue.route_holder;
  const named =
    holder === null || holder === undefined ? "That session" : text.sessionText(holder, undefined);
  const explanation = `${named} has missed its heartbeats without ending, so it still holds this route: what is sent to this issue is stored for it and waits until it answers. Nothing has to be done about it unless it stays this way - a session past the listener's gone horizon leaves the role unheld, and the route then reads as reaching nobody.`;
  return (
    <span
      className={`inline-flex min-w-0 max-w-full items-center gap-1 text-xs ${textMutedOnSurface}`}
      data-testid="issue-route-unresponsive"
      style={{ flexShrink: showRoute ? 1 : 0 }}
      title={explanation}
    >
      {showRoute ? <span className="min-w-0 truncate">{issue.route}:</span> : null}
      <span className="shrink-0">Unresponsive, still reached</span>
      <span className="sr-only">. {explanation}</span>
    </span>
  );
}

function UnreachableRoute({ route, showRoute }: { route: string; showRoute: boolean }): ReactNode {
  const text = useAgentText();
  const sessionId = sessionOfAddress(route);
  const explanation = `${sessionId === undefined ? `No running session holds ${route} right now` : `${text.sessionText(sessionId, undefined)} is not running right now`}, so this issue's messages reach nobody at the moment. A session that is restarting or moving between boxes drops out of the listener for minutes and comes back under the same id, so this is a vacancy only when it is still true on a read ten minutes later; then staff the role, re-route it to a live holder, or clear the route and assign it.`;
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
      {showRoute ? (
        <span className="min-w-0 truncate">
          {sessionId === undefined ? (
            route
          ) : (
            <AgentName inline sessionId={sessionId} storedTitle={undefined} />
          )}
          :
        </span>
      ) : null}
      <span className="shrink-0">
        {sessionId === undefined ? "Nobody holds it right now" : "Not running right now"}
      </span>
      <span className="sr-only">. {explanation}</span>
    </span>
  );
}

/**
 * The live views the shell has mounted (INFRA-812, D5 and D6).
 *
 * A live view does not belong to its route: the person leaves it for the Inbox or another agent
 * and comes back, and what they were writing, where they were reading and the session's own
 * conversation are still there. So the shell mounts the views beside the routes, inside `<main>`
 * (`app.tsx`), and the live route's element renders nothing at all (`LiveViewRoute`).
 *
 * Which views are mounted is the registry's answer, not this component's (`live-views.ts`): at
 * most two, the one in front and the one lingering out the watch window after being left. An
 * entry that sleeps is not here at all, so nothing of it is mounted - no subscription on the
 * live socket, no conversation drawn, no stored-message query, no document title and no read
 * mark - which is what D6 asks for, by construction rather than by a flag each of them reads.
 *
 * The view in front is the page the person is looking at and takes the whole of `<main>`. The
 * one lingering is hidden from them and from a screen reader, but still laid out: it is a
 * scroller holding the reader's place, and a box with no layout at all comes back scrolled to
 * the top, which is the place this change exists to keep. It is taken out of the flow so the
 * page in front is the only thing on screen.
 */
import { lazy, type ReactNode, Suspense, useLayoutEffect } from "react";

import { ErrorBoundary } from "../shell/ErrorBoundary";
import { useLiveViews, useMountedLiveViews } from "./live-views";

const AgentConversationPage = lazy(() =>
  import("./AgentConversationPage").then((module) => ({
    default: module.AgentConversationPage,
  }))
);

/**
 * The live route's element, which renders nothing: `MountedLiveViews` renders the view, outside
 * the routes, so leaving the route lingers the view instead of unmounting it (D5). The route
 * stays in the router because it is what matches the address - without it the live view's
 * address would be the not-found page - and because `routeFillsViewport` is read from the same
 * pattern.
 */
export function LiveViewRoute(): null {
  return null;
}

/**
 * Every mounted live view, each in an error boundary of its own keyed on its session, so one
 * view that throws is one view the reader loses and not the page around it or the other view.
 *
 * `front` is the session whose live view the address names, or `null` while the person is on any
 * other page, which is what starts the view they left lingering.
 */
export function MountedLiveViews({ front }: { front: string | null }): ReactNode {
  const views = useLiveViews();
  // Before paint, so the view the address names is mounted in the commit the address changed in
  // and the live route never shows an empty page of its own making.
  useLayoutEffect(() => {
    views.show(front);
  }, [front, views]);
  const mounted = useMountedLiveViews();
  return (
    <>
      {mounted.map((sessionId) => {
        const inFront = sessionId === front;
        return (
          <div
            aria-hidden={inFront ? undefined : "true"}
            className={inFront ? "flex min-h-0 flex-1 flex-col" : "invisible fixed inset-0"}
            data-front={inFront ? "true" : "false"}
            data-session-id={sessionId}
            data-testid="agent-live-view"
            key={sessionId}
          >
            <ErrorBoundary region="this live view" resetKey={sessionId}>
              {/* The view's own chunk, which the shell loads when the first live view opens and
                  never again; a second view mounts with it already there. Nothing stands in for
                  it while it loads: the live view is the whole page, and a skeleton of another
                  page's shape would be a lie about what is coming. */}
              <Suspense fallback={null}>
                <AgentConversationPage front={inFront} sessionId={sessionId} />
              </Suspense>
            </ErrorBoundary>
          </div>
        );
      })}
    </>
  );
}

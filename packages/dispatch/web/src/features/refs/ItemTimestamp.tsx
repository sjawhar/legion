import { decodeBlockId } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import { type MouseEvent, type ReactNode, useEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { issueQuery } from "../../api/queries";
import { focusVisibleRing } from "../../theme/classes";
import { type ItemRoute, useItemLanding } from "../issue/useItemLanding";
import { useMargin } from "../margin/margin-context";
import { buildReferencePath, type DispatchReferenceRoute } from "./routes";
import { Timestamp } from "./Timestamp";

const LINK_CLASS = `dispatch-inline-link rounded outline-none hover:underline focus-visible:ring-2 ${focusVisibleRing}`;

/** How long a click waits for the reads that say where an item lands before it goes to the
 *  item's address anyway, where the page shows its own loading. */
const LANDING_WAIT_MS = 2_000;

/** Counts clicks on item times, so a click whose reads come back after a later click on another
 *  time does not take the reader away from where they clicked last. */
let latestClick = 0;

/** A click the page follows itself: the main button with no modifier. Cmd or Ctrl (a new tab),
 *  Shift (a new window) and Alt (a download) stay the browser's, as on any link. */
function followsHere(event: MouseEvent<HTMLAnchorElement>): boolean {
  return (
    !event.defaultPrevented &&
    event.button === 0 &&
    !event.metaKey &&
    !event.ctrlKey &&
    !event.shiftKey &&
    !event.altKey
  );
}

function isItemRoute(route: DispatchReferenceRoute): route is ItemRoute {
  return route.kind === "ask" || route.kind === "comment" || route.kind === "message";
}

/** The block a path's `#b-` fragment names, if it names one. */
function blockOf(path: string): string | undefined {
  const fragment = path.split("#")[1];
  return fragment?.startsWith("b-") ? decodeBlockId(fragment.slice(2)) : undefined;
}

/** Goes to `to` as a link would: a push, or a replace when `to` is where the reader already is,
 *  so a click on the item already open adds no history entry for Back to sit on. */
function useFollow(): (to: string) => void {
  const navigate = useNavigate();
  const { hash, pathname, search } = useLocation();
  return (to) => navigate(to, { replace: to === `${pathname}${search}${hash}` });
}

/** The time of an item that has an address of its own - a comment, a reply, a message, an ask or
 *  its answer - as a link to that address (`buildReferencePath` of the route `CopyRefButton`
 *  copies), as a comment's time is on GitHub: Copy link and a new tab get that address. A click
 *  goes where that address lands and marks the item there as any link to it does. The `<time>`
 *  inside keeps the full date on hover. With no route - an item with no address, such as a
 *  direct message on the Agents page - it is the plain time. A time about no one item (a claim's
 *  age, a document's last edit) is a `Timestamp`. */
export function ItemTimestamp({
  at,
  route,
}: {
  at: string;
  route: DispatchReferenceRoute | undefined;
}): ReactNode {
  if (route === undefined) {
    return <Timestamp at={at} />;
  }
  return <ItemLink at={at} route={route} />;
}

function ItemLink({ at, route }: { at: string; route: DispatchReferenceRoute }): ReactNode {
  const follow = useFollow();
  const href = buildReferencePath(route);
  // An issue's item is followed once `Landing` has read where its address lands.
  const [following, setFollowing] = useState<{ click: number; route: ItemRoute }>();
  // The span keeps the link inline wherever the time sits, in a flex row too: an inline link's
  // touch padding (`.dispatch-inline-link`, `styles.css`) then grows its hit box without moving
  // the line.
  return (
    <span>
      <Link
        aria-busy={following === undefined ? undefined : true}
        className={following === undefined ? LINK_CLASS : `${LINK_CLASS} cursor-progress`}
        // The capture phase, before anything under the link sees the click: a read-only
        // document's editor opens a link in its text in a new tab unless the click is handled.
        onClickCapture={(event) => {
          if (!followsHere(event)) return;
          event.preventDefault();
          latestClick += 1;
          if (isItemRoute(route)) setFollowing({ click: latestClick, route });
          else follow(href);
        }}
        to={href}
      >
        <Timestamp at={at} />
      </Link>
      {following === undefined ? null : (
        <Landing
          click={following.click}
          key={following.click}
          onDone={() => setFollowing(undefined)}
          route={following.route}
        />
      )}
    </span>
  );
}

/**
 * Reads where an issue item's address lands, by the issue page's own rule (`useItemLanding`),
 * and goes there once:
 * - a thread beside its document lands on that document's own address, so a reader already on
 *   the document stays on it rather than going through a redirect that opens it again;
 * - every other item lands on its own address with its reads in the cache, so the issue page
 *   shows the item at once rather than "Loading issue…";
 * - a read that fails, or takes longer than `LANDING_WAIT_MS`, lands on the item's address, where
 *   the page says what went wrong or shows its own loading.
 *
 * It goes nowhere once the reader has moved on, or clicked another time, before the reads came
 * back. A landing on a block of the document already open focuses the block through the margin,
 * since the document reads a block fragment only as it opens; on any other document the margin's
 * document holds no such block and the new one reads the fragment itself.
 */
function Landing({
  click,
  onDone,
  route,
}: {
  click: number;
  onDone: () => void;
  route: ItemRoute;
}): null {
  const follow = useFollow();
  const { key: locationKey } = useLocation();
  const [clickedAt] = useState(locationKey);
  const { documentBridge } = useMargin();
  const issue = useQuery(issueQuery(route.key));
  const landing = useItemLanding(route, issue.data);
  const [overdue, setOverdue] = useState(false);
  useEffect(() => {
    const timer = window.setTimeout(() => setOverdue(true), LANDING_WAIT_MS);
    return () => window.clearTimeout(timer);
  }, []);
  const done = useRef(false);
  const stale = locationKey !== clickedAt || click !== latestClick;
  const settled = overdue || issue.isError || (landing !== undefined && landing.kind !== "pending");
  const to =
    landing?.kind === "redirect" && !issue.isError ? landing.to : buildReferencePath(route);
  useEffect(() => {
    if (done.current || !(stale || settled)) return;
    done.current = true;
    onDone();
    if (stale) return;
    follow(to);
    const blockId = blockOf(to);
    if (blockId !== undefined) documentBridge?.focusBlock(blockId);
  }, [documentBridge, follow, onDone, settled, stale, to]);
  return null;
}

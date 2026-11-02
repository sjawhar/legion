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

/** A click the page follows itself: the main button with no modifier, which nothing under the
 *  link took first. Cmd or Ctrl (a new tab), Shift (a new window) and Alt (a download) stay the
 *  browser's, as on any link; a read-only document's editor opens a link in its text, a decision
 *  block's included, in a new tab itself. */
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

/** The time of an item with an address of its own - a comment, a reply, a message, an ask or
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
  const navigate = useNavigate();
  const location = useLocation();
  const { documentBridge } = useMargin();
  const href = buildReferencePath(route);
  // An issue's item is followed once `Landing` has read where its address lands.
  const [following, setFollowing] = useState<ItemRoute>();
  const land = (to: string) => {
    setFollowing(undefined);
    navigate(to);
    // A decision block lands on its block of the document the reader may already be on, which
    // reads the block's fragment only as it opens: on that page the margin focuses the block.
    const [path, fragment] = to.split("#");
    const blockId = fragment?.startsWith("b-") ? decodeBlockId(fragment.slice(2)) : undefined;
    if (blockId !== undefined && path === `${location.pathname}${location.search}`) {
      documentBridge?.focusBlock(blockId);
    }
  };
  return (
    <>
      <Link
        aria-busy={following === undefined ? undefined : true}
        className={following === undefined ? LINK_CLASS : `${LINK_CLASS} cursor-progress`}
        onClick={(event) => {
          if (!followsHere(event)) return;
          event.preventDefault();
          if (isItemRoute(route)) setFollowing(route);
          else navigate(href);
        }}
        to={href}
      >
        <Timestamp at={at} />
      </Link>
      {following === undefined ? null : <Landing onLanded={land} route={following} />}
    </>
  );
}

/** Reads where an issue item's address lands, by the issue page's own rule (`useItemLanding`),
 *  and hands that path on once: a thread beside its document lands on the document's own
 *  address, so a reader already there stays on the page rather than through a redirect that
 *  opens the document again, and every other item lands on its own address, whose reads are
 *  then in the cache, so the issue page shows the item at once rather than "Loading issue…". A
 *  read that fails lands on the item's address too, where the page says what went wrong. */
function Landing({ onLanded, route }: { onLanded: (to: string) => void; route: ItemRoute }): null {
  const issue = useQuery(issueQuery(route.key));
  const landing = useItemLanding(route, issue.data);
  const landed = useRef(false);
  const settled = issue.isError || (landing !== undefined && landing.kind !== "pending");
  const to =
    landing?.kind === "redirect" && !issue.isError ? landing.to : buildReferencePath(route);
  useEffect(() => {
    if (!settled || landed.current) return;
    landed.current = true;
    onLanded(to);
  }, [onLanded, settled, to]);
  return null;
}

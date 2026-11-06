import type { QueryClient } from "@tanstack/react-query";
import { useCallback, useRef } from "react";
import { useNavigate } from "react-router-dom";

import { isPlainClick } from "../refs/picture-viewer";
import { fetchReferencePath, needsSpecSlug } from "../refs/reference-target";
import {
  buildReferencePath,
  type DispatchReferenceRoute,
  parseNodeReference,
} from "../refs/routes";

/** Where a reference opens: React Router's `navigate`. Both document articles take it from
 * `useNavigate`; the picture node view, which is plain DOM and holds no hook, is handed one at
 * its editor's mount (`picture-view.ts`). */
export type NavigateToReference = (path: string) => void;

/** The one a document page hands its editor, which never changes: React Router gives out a new
 * `navigate` on every location change, and the editor is built in an effect that must rebuild
 * for no navigation, so the page keeps the latest in a ref and the editor calls it through
 * this. Both articles read it (`ProofDocument`, `VersionView`) and pass what it answers into
 * `createDoc`, which hands it to the picture node view. */
export function useStableNavigate(): NavigateToReference {
  const navigate = useNavigate();
  const navigateRef = useRef(navigate);
  navigateRef.current = navigate;
  return useCallback((path: string) => navigateRef.current(path), []);
}

/** What the tests below read of a click. React's synthetic mouse event and the DOM's both
 * satisfy it, so the articles hand in the one their JSX handler is given and a plain-DOM
 * listener hands in its own. */
export interface ReferenceClick {
  readonly altKey: boolean;
  readonly button: number;
  readonly ctrlKey: boolean;
  readonly defaultPrevented: boolean;
  readonly metaKey: boolean;
  readonly shiftKey: boolean;
  readonly target: EventTarget | null;
  preventDefault(): void;
}

/** A rendered link mark's `dispatch://` target, if any. `@legion/proof-editor`'s Markdown
 * serializer sanitizes a `dispatch://` href to `""` (Milkdown's link sanitizer only allows
 * http/https/mailto/tel/ftp — a document strangers can edit should never render an
 * attacker-chosen non-http scheme as a clickable href) and tags the anchor with
 * `data-dispatch-href` carrying the original target instead. */
function dispatchHrefOf(anchor: Element): string | null {
  const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href");
  return href?.startsWith("dispatch://") ? href : null;
}

/** Whether a click is the plain primary click that opens a reference inside the app. A modifier
 * click (open in new tab/window), a click of another button, and a drag-selection that happens
 * to end on the link should reach the browser's and the editor's own handling, not be stolen;
 * and so should a click something nearer the target has already answered — the picture's
 * fallback link answers its own click (`picture-view.ts`), and the article sees that same click
 * again as it bubbles. The button and the modifier keys are the one test a picture's own click
 * is held to (`isPlainClick`).
 */
export function opensReference(event: ReferenceClick): boolean {
  if (event.defaultPrevented || !isPlainClick(event)) {
    return false;
  }
  const selection = window.getSelection();
  return selection === null || selection.isCollapsed;
}

/** Opens a reference on the page that holds it, inside the app. */
export function openReference(
  route: DispatchReferenceRoute,
  queryClient: QueryClient,
  navigate: NavigateToReference
): void {
  // A block of a spec pinned to a version opens that version under the spec's slug, which
  // only the issue names; every other reference's path is known at once.
  if (needsSpecSlug(route)) {
    void fetchReferencePath(queryClient, route).then((path) => navigate(path));
    return;
  }
  navigate(buildReferencePath(route));
}

/** The `click` and `auxclick` handler of a document's article, the live editor's
 * (`ProofDocument.tsx`) and a past version's (`VersionView.tsx`) alike: a plain click on a
 * `dispatch://` link opens its target inside the app, instead of falling to the editor library's
 * own link plugin, whose href the serializer emptied, and reloading the page. It takes what it
 * needs — the event, the query client, `navigate` — as arguments, because the picture's fallback
 * link opens a reference through `openReference` from plain DOM, where no hook can be read.
 *
 * Both events, because a middle click raises `auxclick` alone: that click is not a plain one, so
 * it keeps the browser's own meaning and opens the link's real href in a new tab. */
export function handleReferenceClick(
  event: ReferenceClick,
  queryClient: QueryClient,
  navigate: NavigateToReference
): void {
  if (!opensReference(event)) {
    return;
  }
  const target = event.target instanceof Element ? event.target : null;
  const anchor = target?.closest<HTMLAnchorElement>("a") ?? null;
  // The link's target is a node its author named, and opening it is a read, so it is read
  // as a node's reference, whatever the length of its name.
  const href = anchor === null ? null : dispatchHrefOf(anchor);
  const route = href === null ? undefined : parseNodeReference(href);
  if (route === undefined) {
    return;
  }
  event.preventDefault();
  openReference(route, queryClient, navigate);
}

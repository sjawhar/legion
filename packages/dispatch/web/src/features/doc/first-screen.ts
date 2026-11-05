/**
 * The document's opening blocks, as the page's own server drew them into the page it sent.
 *
 * A reader who opens a document's address with a session that may read it is sent its first
 * blocks as HTML (`internal/dispatch/pmdoc/first_screen.go`, `routes/router.go`'s
 * `servePageForDocument`): up to 30 top-level blocks or 16 KB, with each block's `data-block-id`
 * on it, drawn by the same rules this page's own preview draws them by. They are on the screen
 * as the page is parsed - before the entry chunk has downloaded, let alone read the document -
 * which on a phone at 4x CPU throttling is seconds earlier than anything this page can manage
 * (dispatch://INFRA-341, R11).
 *
 * So the page takes them rather than drawing them again: `DocumentPreview` moves these blocks
 * into its own element on its first commit and paints nothing over them until it has the whole
 * document, and the editor swaps in underneath the reader exactly as it always has.
 *
 * The element the server writes carries the address it was drawn for, because a reader can
 * navigate between the page's bytes arriving and the preview mounting: blocks drawn for one
 * document are never handed to another.
 */

/** The element `web/index.html` keeps for the server's draw, after `#root` because `createRoot`
 *  clears what it mounts into. */
const FIRST_SCREEN_ID = "first-screen";

/** The address the server drew the blocks for. */
const ADDRESS_ATTRIBUTE = "data-first-screen-address";

/** The page's current address as the server spells it: its path, decoded, since the server
 *  stamped the decoded path it answered. A path that does not decode is compared as it is. */
export function currentAddress(pathname: string): string {
  try {
    return decodeURIComponent(pathname);
  } catch {
    return pathname;
  }
}

/** Whether the page still carries the server's draw for `address`, which is what lets the
 *  document's own surface draw its preview before its text has arrived: those blocks are the
 *  reader's document, and they are on the screen already. */
export function firstScreenAwaits(address: string): boolean {
  const drawn = document.getElementById(FIRST_SCREEN_ID)?.firstElementChild ?? null;
  return drawn !== null && drawn.getAttribute(ADDRESS_ATTRIBUTE) === address;
}

/** The server's draw for `address`, taken out of the page: its blocks, and the element they were
 *  drawn in removed, so nothing of it is left behind whether or not the caller uses them.
 *  `undefined` where the page carries no draw, or one for another address. */
export function takeFirstScreen(address: string): DocumentFragment | undefined {
  const host = document.getElementById(FIRST_SCREEN_ID);
  const drawn = host?.firstElementChild ?? null;
  if (host === null || drawn === null || drawn.getAttribute(ADDRESS_ATTRIBUTE) !== address) {
    return undefined;
  }
  const blocks = document.createDocumentFragment();
  blocks.append(...drawn.childNodes);
  host.remove();
  return blocks.childNodes.length === 0 ? undefined : blocks;
}

/** Drops the server's draw where nothing will take it: the reader is at another address, so the
 *  blocks belong to a document that is no longer on the screen. */
export function dropFirstScreen(address: string): void {
  const host = document.getElementById(FIRST_SCREEN_ID);
  const drawn = host?.firstElementChild ?? null;
  if (host !== null && (drawn === null || drawn.getAttribute(ADDRESS_ATTRIBUTE) !== address)) {
    host.remove();
  }
}

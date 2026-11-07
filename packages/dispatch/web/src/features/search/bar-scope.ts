/**
 * What the bar will act on, in the words the chip says it in.
 *
 * The bar is one bar from anywhere, so it has to say what it is pointed at: the issue of an issue
 * page, the document of a document page, the issues a reader has selected on the issue list, or
 * the project where the page names nothing narrower. `Tab` narrows that to the block the writing
 * cursor was in as the bar opened, and `Backspace` widens it to the project.
 *
 * This is not the project chip of the filter bar, which narrows the search and carries its own
 * `Remove Project INFRA` control: that chip says which items are searched, this one says what the
 * bar acts on. They sit on either side of the box, and only the filter chip can be removed.
 */
import { BLOCK_ID_ATTRIBUTE } from "../doc/block-id";
import {
  type ProjectDocumentRoute,
  parseIssuePath,
  parseProjectPath,
  routeProjectOf,
} from "../refs/routes";

/** How much of a block's own text the chip names it by. */
const BLOCK_TEXT_MAX = 60;
/** How many selected issues the chip names before it counts the rest. */
const NAMED_SELECTION_MAX = 3;

/**
 * The issues the reader has selected on the issue list (`features/project/IssueList.tsx`), which
 * is the one page with a selection of its own. The list writes it while it is mounted and clears
 * it as it goes; the bar reads it once, as it opens, so no render of the list moves a chip the
 * reader is looking at.
 */
let selectedIssues: readonly string[] = [];

export function setSelectedIssues(keys: readonly string[]): void {
  selectedIssues = keys;
}

export function readSelectedIssues(): readonly string[] {
  return selectedIssues;
}

/** `a`, `a and b`, `a, b and c`, then `a, b, c and 2 more`: a chip naming every key of a long
 *  selection would be wider than the bar. */
function joinKeys(keys: readonly string[]): string {
  const named = keys.slice(0, NAMED_SELECTION_MAX);
  const rest = keys.length - named.length;
  const last = rest === 0 ? named.pop() : `${rest} more`;
  return named.length === 0 ? (last ?? "") : `${named.join(", ")} and ${last}`;
}

/** A block named by the start of its own text, which is what a reader recognises it by: runs of
 *  whitespace collapsed, cut at a word and closed with an ellipsis. */
function blockText(text: string): string {
  const collapsed = text.replace(/\s+/gu, " ").trim();
  if (collapsed.length <= BLOCK_TEXT_MAX) {
    return collapsed;
  }
  const cut = collapsed.slice(0, BLOCK_TEXT_MAX);
  const lastSpace = cut.lastIndexOf(" ");
  return `${(lastSpace === -1 ? cut : cut.slice(0, lastSpace)).trimEnd()}…`;
}

/**
 * The block the writing cursor is in, as the DOM has it right now. The bar reads this on the
 * render that opens it, while focus is still where the reader left it: a moment later the dialog
 * has taken the focus and the document's selection is no longer the page's.
 *
 * A block is a block of a Dispatch document, the thing a `#b-` reference names, so it is found by
 * the id the editor stamps on it (`BLOCK_ID_ATTRIBUTE`). The cursor has to be inside the writing
 * surface: a reader scrolling an issue page has a selection somewhere too, and that is no cursor.
 */
export function scopeAtCursor(): string | undefined {
  const focused = document.activeElement;
  if (!(focused instanceof HTMLElement) || !focused.isContentEditable) {
    return undefined;
  }
  const anchor = window.getSelection()?.anchorNode ?? null;
  const from = anchor instanceof Element ? anchor : (anchor?.parentElement ?? focused);
  const inside = focused.contains(from) ? from : focused;
  const block = inside.closest<HTMLElement>(`[${BLOCK_ID_ATTRIBUTE}]`);
  const text = block === null ? "" : blockText(block.textContent ?? "");
  return text === "" ? undefined : `the block “${text}”`;
}

/** The project of the page the bar was opened from, which `Backspace` widens the chip to. */
export function scopeOfProject(pathname: string): string | undefined {
  const project = routeProjectOf(pathname);
  return project === undefined ? undefined : `the project ${project}`;
}

/**
 * What the page itself puts under the bar: the issue of an issue page (its key, which names it
 * everywhere in Dispatch), the document of a project document page by its own name, the issues
 * the reader selected on the issue list, and otherwise the project. A page belonging to no
 * project - the Inbox, Agents, Settings - puts nothing there, and the bar shows no chip.
 */
export function scopeOfPage(
  pathname: string,
  search: string,
  documentName: string | undefined,
  selected: readonly string[]
): string | undefined {
  const issue = parseIssuePath(pathname, search);
  if (issue !== undefined) {
    return issue.key;
  }
  const project = parseProjectPath(pathname, search);
  if (project === undefined) {
    return undefined;
  }
  if (project.kind === "document") {
    return documentName ?? project.slug;
  }
  return selected.length === 0 ? scopeOfProject(pathname) : joinKeys(selected);
}

/** The issue the page under the bar is on, whatever tab of it, and undefined on every other
 *  page: the issue an offer acts on, as against the chip's words for it. */
export function issueOfPage(pathname: string, search: string): string | undefined {
  return parseIssuePath(pathname, search)?.key;
}

/** The project document the page shows, whose name the chip reads it by. */
export function documentRouteOf(
  pathname: string,
  search: string
): ProjectDocumentRoute | undefined {
  const route = parseProjectPath(pathname, search);
  return route?.kind === "document" ? route : undefined;
}

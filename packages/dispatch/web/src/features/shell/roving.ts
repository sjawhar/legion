/** The nearest element matching `selector` at or above `node`; `null` off any such element. */
export function closestMatching(node: Element | null, selector: string): HTMLElement | null {
  return node?.closest<HTMLElement>(selector) ?? null;
}

/**
 * The element matching `selector` that holds keyboard focus *itself* - not one merely containing
 * a focused control. Every roving list asks this of its own rows: a key bound to the row acts
 * while the reader is on the row, and stays out of the way while they are in something inside it.
 */
export function focusedMatching(selector: string): HTMLElement | null {
  const active = document.activeElement;
  return active instanceof HTMLElement && active.matches(selector) ? active : null;
}

/**
 * The rows of `nodes` a reader can actually reach, for the keys that step over them and for
 * the `when` that offers those keys - one rule, so the two cannot disagree.
 *
 * A row inside a collapsed `details` is in the DOM, is not rendered, and can take no focus at
 * all, so counting it would dead-end the keys on the row before the band and strand every row
 * past it. A closed band is the whole of it for the three lists that rove: the project List
 * keeps each status band in a `details` (`project/IssueList.tsx`), while the Inbox renders no
 * row of a folded band and Architecture renders only the current level's rows.
 * `checkVisibility()` would answer this and more, and needs Safari 17.4 / Chrome 105 /
 * Firefox 106; a focus rule is not worth raising the browsers Dispatch runs on.
 */
export function reachableRows(nodes: readonly HTMLElement[]): HTMLElement[] {
  return nodes.filter((node) => node.closest("details:not([open])") === null);
}

/**
 * Moves keyboard focus one step through the rows of `nodes` the reader can reach: from
 * `current` to its neighbour, clamped at both ends; when nothing in the list holds focus, to
 * the first node going down and the last going up. Nothing happens when none is reachable.
 */
export function roveFocus(
  nodes: readonly HTMLElement[],
  current: HTMLElement | null,
  delta: 1 | -1
): void {
  const reachable = reachableRows(nodes);
  if (reachable.length === 0) {
    return;
  }
  const at = current === null ? -1 : reachable.indexOf(current);
  const next =
    at === -1
      ? delta === 1
        ? 0
        : reachable.length - 1
      : Math.max(0, Math.min(reachable.length - 1, at + delta));
  reachable[next]?.focus();
}

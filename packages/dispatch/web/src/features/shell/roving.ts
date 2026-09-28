/** The nearest element matching `selector` at or above `node`; `null` off any such element. */
export function closestMatching(node: Element | null, selector: string): HTMLElement | null {
  return node?.closest<HTMLElement>(selector) ?? null;
}

/**
 * Moves keyboard focus one step through the nodes of `nodes` the reader can see: from `current`
 * to its neighbour, clamped at both ends; when nothing in the list holds focus, to the first
 * node going down and the last going up. Nothing happens on an empty list.
 *
 * A node the reader cannot see is not a step. A list's rows can be in the DOM and unrendered -
 * the project List keeps each status band in a `details`, and a closed one hides its rows -
 * and such a row can take no focus at all, so counting it would dead-end the keys on the row
 * before it and strand every row past the closed band.
 */
export function roveFocus(
  nodes: readonly HTMLElement[],
  current: HTMLElement | null,
  delta: 1 | -1
): void {
  const visible = nodes.filter((node) => node.checkVisibility());
  if (visible.length === 0) {
    return;
  }
  const at = current === null ? -1 : visible.indexOf(current);
  const next =
    at === -1
      ? delta === 1
        ? 0
        : visible.length - 1
      : Math.max(0, Math.min(visible.length - 1, at + delta));
  visible[next]?.focus();
}

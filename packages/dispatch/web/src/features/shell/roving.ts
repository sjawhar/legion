/** The nearest element matching `selector` at or above `node`; `null` off any such element. */
export function closestMatching(node: Element | null, selector: string): HTMLElement | null {
  return node?.closest<HTMLElement>(selector) ?? null;
}

/**
 * Moves keyboard focus one step through `nodes`: from `current` to its neighbour, clamped at
 * both ends; when nothing in the list holds focus, to the first node going down and the last
 * going up. Nothing happens on an empty list.
 */
export function roveFocus(
  nodes: readonly HTMLElement[],
  current: HTMLElement | null,
  delta: 1 | -1
): void {
  if (nodes.length === 0) {
    return;
  }
  const at = current === null ? -1 : nodes.indexOf(current);
  const next =
    at === -1
      ? delta === 1
        ? 0
        : nodes.length - 1
      : Math.max(0, Math.min(nodes.length - 1, at + delta));
  nodes[next]?.focus();
}

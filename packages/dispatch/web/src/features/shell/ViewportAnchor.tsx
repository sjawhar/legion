import { Component, type ReactNode, type RefObject } from "react";

export interface AnchorRow {
  id: string;
  /** How the row was chosen: keyboard focus within it, the pointer resting on it, or, failing
   *  both, the row nearest the list's visible centre. */
  via: "center" | "focus" | "pointer";
}

interface Snapshot extends AnchorRow {
  /** The row's top, in viewport pixels, immediately before the commit. */
  top: number;
}

export interface ViewportAnchorProps {
  as?: "div" | "section";
  children: ReactNode;
  className?: string;
  /** When false at commit time, the commit is left to shift the view (the Conversation while it
   *  follows the latest turn). Defaults to always compensating. */
  enabled?: () => boolean;
  /** Attribute naming each row and carrying its id, e.g. `data-inbox-row`. */
  item: string;
  label?: string;
  /** Receives the rendered root element, for a host that also queries the rows itself. */
  rootRef?: RefObject<HTMLElement | null>;
}

const probeSteps = 5;
const probeStepPx = 8;

// An O(1) hit test for the row nearest the centre of the list's own visible portion, rather than
// a getBoundingClientRect scan over every row (hundreds of forced layout reads per update on a
// long list). The probe point is derived from the list's bounding rect - not
// window.innerWidth/innerHeight - because the main column is rarely centred in (or as wide as)
// the viewport: the sidebar and margin panel narrow it well before the window itself is narrow,
// and hit-testing the window's centre can land in a neighbouring column entirely, missing every
// probe. elementFromPoint at the exact centre can still miss within the column - landing on a
// gap, heading, or composer - in which case this probes downward first, then upward, in small
// steps: a content change lands between two rows more often than exactly on one, and probing
// downward first means the answer for a point in the gap is consistently "whichever row is
// below", matching reading order.
function rowAtCenter(root: HTMLElement, selector: string): HTMLElement | null {
  const rect = root.getBoundingClientRect();
  const x = rect.left + rect.width / 2;
  const visibleTop = Math.max(rect.top, 0);
  const visibleBottom = Math.min(rect.bottom, window.innerHeight);
  const y = (visibleTop + visibleBottom) / 2;
  const hit = (offset: number): HTMLElement | null =>
    document.elementFromPoint(x, y + offset)?.closest<HTMLElement>(selector) ?? null;

  let found = hit(0);
  for (let step = 1; found === null && step <= probeSteps; step += 1) {
    found = hit(step * probeStepPx);
  }
  for (let step = 1; found === null && step <= probeSteps; step += 1) {
    found = hit(-step * probeStepPx);
  }
  return found !== null && root.contains(found) ? found : null;
}

function rowWithin(root: HTMLElement, selector: string, node: Element | null): HTMLElement | null {
  const row = node?.closest<HTMLElement>(selector) ?? null;
  return row !== null && root.contains(row) ? row : null;
}

/**
 * Keeps the row the reader is currently on pinned to its viewport position across commits that
 * move it or shift content around it: a row inserted or removed above, a row moving between
 * sections, a banner appearing. The current row is the one with keyboard focus within it, else
 * the one under the pointer, else the one nearest the list's visible centre - the first two hold
 * even at the top of the page (the hand is on that row), the centre only once the reader has
 * scrolled, since a reader at the top is looking for arrivals and should see them land.
 *
 * Rows are the elements matching `[item]`, identified by that attribute's value, and must keep
 * their identity across the commit (a remounted row is a new node and cannot be found again).
 *
 * A class because `getSnapshotBeforeUpdate` has no hook equivalent, and the measurement must be
 * taken in the commit phase, immediately before React mutates the DOM: a render can complete and
 * then wait for its commit (React yields between the two for non-urgent updates), and anything
 * measured during that render is stale by the time the commit lands if the reader scrolled or a
 * click moved the page in between - the Conversation's Jump to latest was undone exactly this
 * way by a render-phase measurement. The same timing is why nothing cached from a `scroll`
 * handler can be trusted here: a scroll performed immediately before a live update may not have
 * dispatched its event before React commits. `componentDidUpdate` then compensates exactly once,
 * before paint.
 */
export class ViewportAnchor extends Component<ViewportAnchorProps, object, Snapshot | null> {
  private root: HTMLElement | null = null;
  private hovered: HTMLElement | null = null;

  private readonly pointerOver = (event: PointerEvent): void => {
    const root = this.root;
    if (root === null || !(event.target instanceof Element) || !root.contains(event.target)) {
      return;
    }
    this.hovered = rowWithin(root, `[${this.props.item}]`, event.target);
  };

  private readonly pointerOut = (event: PointerEvent): void => {
    const row = this.hovered;
    if (
      row !== null &&
      !(event.relatedTarget instanceof Node && row.contains(event.relatedTarget))
    ) {
      this.hovered = null;
    }
  };

  componentDidMount(): void {
    document.addEventListener("pointerover", this.pointerOver);
    document.addEventListener("pointerout", this.pointerOut);
  }

  componentWillUnmount(): void {
    document.removeEventListener("pointerover", this.pointerOver);
    document.removeEventListener("pointerout", this.pointerOut);
  }

  /** The row the reader's hand is on: focus within a row, else the pointer resting on one. No
   *  layout reads, so a parent may call it on every render. */
  interacted(): (AnchorRow & { row: HTMLElement }) | null {
    const root = this.root;
    if (root === null) return null;
    const selector = `[${this.props.item}]`;
    const focused = rowWithin(root, selector, document.activeElement);
    const hovered = this.hovered !== null && root.contains(this.hovered) ? this.hovered : null;
    const chosen =
      focused !== null
        ? { row: focused, via: "focus" as const }
        : hovered !== null
          ? { row: hovered, via: "pointer" as const }
          : null;
    return chosen === null ? null : { ...chosen, id: this.idOf(chosen.row) };
  }

  /** `interacted()`, else - only once the reader has scrolled - the row nearest the list's
   *  visible centre (a hit-test probe: layout reads). The row a commit will anchor to. */
  current(): (AnchorRow & { row: HTMLElement }) | null {
    const interacted = this.interacted();
    if (interacted !== null) return interacted;
    const root = this.root;
    if (root === null || window.scrollY <= 0) return null;
    const row = rowAtCenter(root, `[${this.props.item}]`);
    return row === null ? null : { id: this.idOf(row), row, via: "center" };
  }

  private idOf(row: HTMLElement): string {
    const id = row.getAttribute(this.props.item);
    if (id === null) {
      throw new Error(`viewport anchor row is missing its ${this.props.item} value`);
    }
    return id;
  }

  getSnapshotBeforeUpdate(): Snapshot | null {
    const { enabled } = this.props;
    if (enabled !== undefined && !enabled()) return null;
    const current = this.current();
    if (current === null) return null;
    return { id: current.id, top: current.row.getBoundingClientRect().top, via: current.via };
  }

  componentDidUpdate(
    _previousProps: Readonly<ViewportAnchorProps>,
    _previousState: Readonly<object>,
    snapshot: Snapshot | null
  ): void {
    const root = this.root;
    if (snapshot === null || root === null) return;
    const element = root.querySelector<HTMLElement>(
      `[${this.props.item}="${CSS.escape(snapshot.id)}"]`
    );
    if (element === null) return;
    const delta = element.getBoundingClientRect().top - snapshot.top;
    if (delta !== 0) {
      window.scrollBy(0, delta);
    }
  }

  render(): ReactNode {
    const { as: Tag = "div", children, className, label, rootRef } = this.props;
    return (
      <Tag
        aria-label={label}
        className={className}
        ref={(node: HTMLElement | null) => {
          this.root = node;
          if (rootRef !== undefined) rootRef.current = node;
        }}
      >
        {children}
      </Tag>
    );
  }
}

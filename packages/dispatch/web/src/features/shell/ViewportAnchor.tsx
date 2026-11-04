import { Component, type ReactNode, type RefObject } from "react";

import { refPreviewCardContains, refPreviewOpenedIn } from "../refs/ref-preview-card";

export interface AnchorRow {
  id: string;
  /** How the row was chosen: keyboard focus within it, the pointer resting on it, or, failing
   *  both, the row nearest the list's visible centre. */
  via: "center" | "focus" | "pointer";
}

interface Snapshot extends AnchorRow {
  /** The row's `group` attribute value immediately before the commit; null without one. */
  group: string | null;
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
  /** Attribute naming the group a row sits in, e.g. `data-inbox-section`. A row whose value
   *  changed across a commit was relocated - it left its place for another group - rather than
   *  shifted by content around it, and the view is not dragged after it. */
  group?: string;
  /** Attribute naming each row and carrying its id, e.g. `data-inbox-row`. */
  item: string;
  label?: string;
  /** Receives the rendered root element, for a host that also queries the rows itself. */
  rootRef?: RefObject<HTMLElement | null>;
  /** The element the rows scroll in, when they scroll in one rather than the document (the
   *  Inbox beside its reading pane, where each column scrolls in itself). The anchor then reads
   *  and moves that element's `scrollTop`, and measures the list against its box. With no ref,
   *  or one holding null, the window scrolls the rows as it always has. */
  scroller?: RefObject<HTMLElement | null>;
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
// below", matching reading order. What is visible is the window's own box, or `scroller`'s when
// the rows scroll in one: a list scrolled inside an element reaches above and below that
// element's edges, not the window's, and probing a point the scroller clips lands on whatever
// covers it instead.
function rowAtCenter(
  root: HTMLElement,
  selector: string,
  scroller: HTMLElement | null
): HTMLElement | null {
  const rect = root.getBoundingClientRect();
  const x = rect.left + rect.width / 2;
  const box = scroller === null ? null : scroller.getBoundingClientRect();
  const visibleTop = Math.max(rect.top, box === null ? 0 : box.top);
  const visibleBottom = Math.min(rect.bottom, box === null ? window.innerHeight : box.bottom);
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
 * shift content around it: a row inserted or removed above, a reorder within its section, a
 * banner appearing. The current row is the one with keyboard focus within it, else the one under
 * the pointer, else the one nearest the list's visible centre - the first two hold even at the
 * top of the page (the hand is on that row), the centre only once the reader has scrolled, since
 * a reader at the top is looking for arrivals and should see them land. A row that itself leaves
 * its place for another `group` (a section) is not followed: the reader's place is where the row
 * was, not where it went. The pointer moving from a row onto the hover card a reference in that
 * row opened keeps that row the current one while the card is open: the card is the row's own
 * doing and sits outside the list only because it is `fixed`.
 *
 * Rows are the elements matching `[item]`, identified by that attribute's value, and must keep
 * their identity across the commit (a remounted row is a new node and cannot be found again).
 * The document scrolls them unless the host names a `scroller`, in which case that element's
 * `scrollTop` is both what is read and what is moved, and the window's scroll is left alone.
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
  /** Set while the pointer left the hovered row for the hover card a reference in that row
   *  opened. The card is `fixed` and portaled into `document.body`
   *  (`features/refs/RefPreview.tsx`), so no later boundary event on this root says where the
   *  pointer went from there, and the row is held as the reader's place only for as long as
   *  that card is open - once the pointer leaves it the card closes itself
   *  (`REF_PREVIEW_CLOSE_DELAY_MS`) and `interacted` lets the row go. */
  private onOwnCard = false;
  /** The row the reader's hand is on and the viewport top they last saw it at - where every
   *  compensation puts it back. It is read rather than re-measured because the view can be
   *  wrong when a commit arrives: a change this component's own commits never see has already
   *  moved the row. A card below the hand answering itself, a thread body arriving, Markdown
   *  finishing - each is a child's own commit, which re-renders the child alone, so
   *  `getSnapshotBeforeUpdate` is never called for it. Where the rows scroll in the document
   *  the browser's own scroll anchoring covers that; in an element the host names (`scroller`)
   *  it does not, which is what `contentResized` stands in for. Measured in the Inbox drawer:
   *  the answered row's card shrinks by a card's height one commit before the list drops the
   *  row, and the reader's row ended exactly that far out. */
  private held: { id: string; top: number } | null = null;
  private contentSize: ResizeObserver | null = null;
  /** Set by a scroll this component made, so the scroll event it raises is not read as the
   *  reader moving the view. Scroll events run before resize observations in a frame, so
   *  without this the compensation's own event would record the place it just corrected. */
  private selfScrolled = false;

  /** Where the pointer last was, in viewport pixels. A row moving under a pointer that has not
   *  moved makes the browser dispatch `pointerout`/`pointerover` again, with the same
   *  coordinates: that is the list moving under the hand, which is the thing being compensated
   *  for, so it must not be read as the reader looking somewhere new. */
  private pointerAt: { x: number; y: number } | null = null;

  /** Records where the hand's row sits now as the place to keep it. Called where the view is
   *  the reader's own: the pointer moving onto a row or off one, and a scroll they made. */
  private readonly rememberHeld = (): void => {
    const current = this.interacted();
    this.held =
      current === null ? null : { id: current.id, top: current.row.getBoundingClientRect().top };
  };

  /** True when this pointer event is the hand moving rather than the list moving under it. */
  private handMoved(event: PointerEvent): boolean {
    const at = this.pointerAt;
    this.pointerAt = { x: event.clientX, y: event.clientY };
    return at === null || at.x !== event.clientX || at.y !== event.clientY;
  }

  /** Moves the box this anchor owns and remembers that it did. */
  private scrollBy(delta: number): void {
    const scroller = this.props.scroller?.current ?? null;
    if (scroller === null) {
      const before = window.scrollY;
      window.scrollBy(0, delta);
      this.selfScrolled = window.scrollY !== before;
      return;
    }
    const before = scroller.scrollTop;
    scroller.scrollTop += delta;
    this.selfScrolled = scroller.scrollTop !== before;
  }

  private readonly viewScrolled = (event: Event): void => {
    const scroller = this.props.scroller?.current ?? null;
    const mine = scroller === null ? event.target === document : event.target === scroller;
    if (!mine) return;
    if (this.selfScrolled) {
      this.selfScrolled = false;
      return;
    }
    this.rememberHeld();
  };

  /** The root's content changed height outside a commit of this component's. Puts the hand's
   *  row back where the reader last saw it, in the box the host named; with no such box the
   *  document's own scroll anchoring owns this and nothing here runs. */
  private readonly contentResized = (): void => {
    const held = this.held;
    const root = this.root;
    if (this.props.scroller?.current == null || held === null || root === null) return;
    const element = root.querySelector<HTMLElement>(
      `[${this.props.item}="${CSS.escape(held.id)}"]`
    );
    if (element === null) {
      this.held = null;
      return;
    }
    const delta = element.getBoundingClientRect().top - held.top;
    if (delta !== 0) this.scrollBy(delta);
  };

  componentDidMount(): void {
    // Capture, because `scroll` does not bubble: one listener catches the window's and the
    // named box's alike, and the handler reads a rect only for the box this anchor owns.
    window.addEventListener("scroll", this.viewScrolled, true);
  }

  componentWillUnmount(): void {
    window.removeEventListener("scroll", this.viewScrolled, true);
    this.contentSize?.disconnect();
    this.contentSize = null;
  }

  // Both listeners hang off the rendered root, not `document`: `pointerover`/`pointerout` bubble,
  // so a pointer entering a row still reaches the root, and leaving a row for anything outside
  // still fires `pointerout` on the row being left with the same `relatedTarget`.
  private readonly pointerOver = (event: PointerEvent): void => {
    const root = this.root;
    if (root === null || !(event.target instanceof Element)) {
      return;
    }
    this.hovered = rowWithin(root, `[${this.props.item}]`, event.target);
    this.onOwnCard = false;
    if (this.handMoved(event)) this.rememberHeld();
  };

  // A pointer moving from a row onto the card that row's own reference opened has left nothing
  // the reader can see leaving: the card is the row's doing, drawn outside the list only
  // because it is `fixed`. Releasing the row there is what moved the row under the reader when
  // a refetch landed with the pointer on the card.
  private readonly pointerOut = (event: PointerEvent): void => {
    const row = this.hovered;
    if (
      row === null ||
      (event.relatedTarget instanceof Node && row.contains(event.relatedTarget))
    ) {
      return;
    }
    if (refPreviewCardContains(event.relatedTarget) && refPreviewOpenedIn(row)) {
      this.onOwnCard = true;
      return;
    }
    this.hovered = null;
    this.onOwnCard = false;
    if (this.handMoved(event)) this.rememberHeld();
  };

  private readonly setRoot = (node: HTMLElement | null): void => {
    if (this.root !== null) {
      this.root.removeEventListener("pointerover", this.pointerOver);
      this.root.removeEventListener("pointerout", this.pointerOut);
    }
    this.contentSize?.disconnect();
    this.contentSize = null;
    this.root = node;
    if (node !== null) {
      node.addEventListener("pointerover", this.pointerOver);
      node.addEventListener("pointerout", this.pointerOut);
      // Not every environment the unit tests run in has one (happy-dom); there the hold is the
      // commit path alone, which is what those tests exercise.
      if (typeof ResizeObserver !== "undefined") {
        this.contentSize = new ResizeObserver(this.contentResized);
        this.contentSize.observe(node);
      }
    }
    const { rootRef } = this.props;
    if (rootRef !== undefined) rootRef.current = node;
  };

  /** The row the reader's hand is on: focus within a row, else the pointer resting on one. No
   *  layout reads, so a parent may call it on every render. */
  interacted(): (AnchorRow & { row: HTMLElement }) | null {
    const root = this.root;
    if (root === null) return null;
    const selector = `[${this.props.item}]`;
    const focused = rowWithin(root, selector, document.activeElement);
    const resting =
      this.hovered !== null && (!this.onOwnCard || refPreviewOpenedIn(this.hovered))
        ? this.hovered
        : null;
    const hovered = resting !== null && root.contains(resting) ? resting : null;
    const chosen =
      focused !== null
        ? { row: focused, via: "focus" as const }
        : hovered !== null
          ? { row: hovered, via: "pointer" as const }
          : null;
    return chosen === null ? null : { ...chosen, id: this.idOf(chosen.row) };
  }

  /** `interacted()`, else - only once the reader has scrolled - the row nearest the list's
   *  visible centre (a hit-test probe: layout reads). The row a commit will anchor to. The box
   *  that scrolls is `scroller`'s element while the host gave one and it holds one, else the
   *  window, and `componentDidUpdate` moves whichever this read. */
  current(): (AnchorRow & { row: HTMLElement }) | null {
    const interacted = this.interacted();
    if (interacted !== null) return interacted;
    const root = this.root;
    if (root === null) return null;
    const scroller = this.props.scroller?.current ?? null;
    if ((scroller === null ? window.scrollY : scroller.scrollTop) <= 0) return null;
    const row = rowAtCenter(root, `[${this.props.item}]`, scroller);
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
    const { enabled, group } = this.props;
    if (enabled !== undefined && !enabled()) return null;
    const current = this.current();
    if (current === null) return null;
    // Where the reader last saw the row they have their hand on, not where it sits now: a
    // change no commit of this component's saw may already have moved it (`held`), and
    // measuring again here would take that drift for the place to keep.
    const held = this.held;
    return {
      group: group === undefined ? null : current.row.getAttribute(group),
      id: current.id,
      top:
        held !== null && held.id === current.id
          ? held.top
          : current.row.getBoundingClientRect().top,
      via: current.via,
    };
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
    const { group } = this.props;
    if (group !== undefined && element.getAttribute(group) !== snapshot.group) return;
    const delta = element.getBoundingClientRect().top - snapshot.top;
    if (delta !== 0) this.scrollBy(delta);
  }

  render(): ReactNode {
    const { as: Tag = "div", children, className, label } = this.props;
    return (
      <Tag aria-label={label} className={className} ref={this.setRoot}>
        {children}
      </Tag>
    );
  }
}

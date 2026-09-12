import { Component, type ReactNode } from "react";

interface ReadingAnchor {
  seq: number;
  top: number;
}

interface ReaderPositionProps {
  children: ReactNode;
  className: string;
  shouldCompensate: () => boolean;
}

const probeSteps = 5;
const probeStepPx = 8;

// An O(1) hit test for the turn nearest the center of the Conversation's own visible portion,
// rather than a getBoundingClientRect scan over every loaded turn (hundreds of forced layout
// reads per scroll tick on a long conversation). The probe point is derived from the Conversation
// section's own bounding rect — not window.innerWidth/innerHeight — because the conversation
// column is rarely centered in (or as wide as) the viewport: the sidebar and margin panel narrow
// it well before the window itself is narrow, and hit-testing the window's center can land in a
// neighboring column entirely, missing every probe. elementFromPoint at the exact center can still
// miss within the column itself — landing on a gap, divider, or composer below the list — in which
// case this probes downward first, then upward, in small steps: a content change lands between two
// turns more often than exactly on one, and probing downward first means the reader's answer for a
// point in the gap is consistently "whichever turn is below", matching reading order.
function measureAnchor(root: HTMLElement): ReadingAnchor | null {
  const rect = root.getBoundingClientRect();
  const x = rect.left + rect.width / 2;
  const visibleTop = Math.max(rect.top, 0);
  const visibleBottom = Math.min(rect.bottom, window.innerHeight);
  const y = (visibleTop + visibleBottom) / 2;
  const hit = (offset: number): HTMLElement | null =>
    document.elementFromPoint(x, y + offset)?.closest<HTMLElement>("[data-event-seq]") ?? null;

  let found = hit(0);
  for (let step = 1; found === null && step <= probeSteps; step += 1) {
    found = hit(step * probeStepPx);
  }
  for (let step = 1; found === null && step <= probeSteps; step += 1) {
    found = hit(-step * probeStepPx);
  }
  if (found === null || !root.contains(found)) {
    return null;
  }
  return { seq: Number(found.dataset.eventSeq), top: found.getBoundingClientRect().top };
}

/**
 * The Conversation never moves the reader's attention: keeps the turn they are looking at pinned
 * to its viewport position across renders that shift content above it (a new turn arriving while
 * the reader is browsing history, a pin reflow, the failed-operations banner appearing, or the
 * unread divider moving as a turn is marked read). Readers already at the top are pinned to the
 * latest turn, so a new turn simply appears in place there without any adjustment.
 *
 * `getSnapshotBeforeUpdate` is essential here: a scroll performed immediately before an SSE update
 * may not dispatch its browser `scroll` event before React commits that update. It captures the
 * actual viewport anchor before the DOM reflow, then compensates exactly once after the commit.
 */
export class ReaderPosition extends Component<ReaderPositionProps, object, ReadingAnchor | null> {
  private root: HTMLElement | null = null;

  getSnapshotBeforeUpdate(): ReadingAnchor | null {
    if (this.root === null || window.scrollY <= 0 || !this.props.shouldCompensate()) {
      return null;
    }
    return measureAnchor(this.root);
  }

  componentDidUpdate(
    _previousProps: Readonly<ReaderPositionProps>,
    _previousState: Readonly<object>,
    anchor: ReadingAnchor | null
  ): void {
    if (anchor === null || this.root === null) {
      return;
    }
    const element = this.root.querySelector<HTMLElement>(`[data-event-seq="${anchor.seq}"]`);
    if (element === null) {
      return;
    }
    const delta = element.getBoundingClientRect().top - anchor.top;
    if (delta !== 0) {
      window.scrollBy(0, delta);
    }
  }

  render(): ReactNode {
    return (
      <section
        aria-label="Conversation"
        className={this.props.className}
        ref={(node) => {
          this.root = node;
        }}
      >
        {this.props.children}
      </section>
    );
  }
}

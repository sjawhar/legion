/**
 * How long the Inbox's reading column keeps the document it opened at the section it was opened
 * for.
 *
 * The column scrolls a live document to the heading of the section holding the question or the
 * anchored block it shows, and one scroll is never enough: the editor draws its blocks in
 * pieces, a diagram or a picture draws later still, and every block that lands above the
 * heading pushes it down again. So the placement is applied on every change of the drawn
 * document rather than once - and the one thing that ends it is the reader moving the column
 * themselves. This is the rule the document's own focus re-run follows
 * (`features/doc/refocus-after-diagrams.ts`), without its single shot and read on the column
 * rather than on the window: picking another row in the list beside it is not leaving the
 * section, and the column that pick opens is placed like any other.
 */

/** What a reader does to move on from where the column put them. */
const READER_ACTIONS = ["pointerdown", "wheel", "touchstart", "keydown"] as const;

export interface SectionHold {
  /** Lets go and stops listening: the column is the reader's from here on. Called when the
   *  column closes, and by a placement of the reader's own - the head's **Skip to the answer** -
   *  which is not in the column and so is not seen as a press in it. */
  readonly release: () => void;
  /** Whether the column may still be put back at its section. */
  readonly held: () => boolean;
}

/** Holds `column` at its section until the reader presses, scrolls, touches or types in it. */
export function holdSection(column: HTMLElement): SectionHold {
  let held = true;
  const release = () => {
    if (!held) {
      return;
    }
    held = false;
    for (const type of READER_ACTIONS) {
      column.removeEventListener(type, release, true);
    }
  };
  for (const type of READER_ACTIONS) {
    column.addEventListener(type, release, { capture: true, passive: true });
  }
  return { held: () => held, release };
}

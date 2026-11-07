import { lazy, type ReactNode, Suspense } from "react";
import { createPortal } from "react-dom";

import type { ViewBlockSlot } from "./view-slots";

/**
 * The block's renderer, its frame budget and its attribute reading, behind `import()`: a page
 * with no `view` block on it fetches none of them, and a page that holds one fetches them as the
 * block's shell is drawn, the way the editor library and the delivery charts load.
 *
 * Nothing of the frame starts with the import. The chunk only brings the component, which then
 * registers the slot with the budget and reads nothing until the budget says the block is near
 * enough to run (`frame-budget.ts`), so a block far down the page still reads nothing until the
 * reader scrolls to it.
 */
const ViewBlock = lazy(() =>
  import("./ViewBlock").then((module) => ({ default: module.ViewBlock }))
);

/** A `ViewBlock` portalled into each of these frame slots, which is how both the body renderer
 *  and the document editors mount one. The slot is drawn and the caption beside it is on the
 *  page before this runs, so the wait for the chunk shows the block as its budget-held form
 *  does: the shell, with nothing in the frame slot yet. */
export function viewPortals(slots: readonly ViewBlockSlot[]): ReactNode[] {
  return slots.map((slot) =>
    createPortal(
      <Suspense fallback={null}>
        <ViewBlock slot={slot} />
      </Suspense>,
      slot.host,
      slot.key
    )
  );
}

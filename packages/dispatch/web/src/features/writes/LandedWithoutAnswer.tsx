import type { ReactNode } from "react";

import { textSecondaryOnSurface } from "../../theme/classes";

/**
 * The one line a create form shows when Dispatch made what it asked for and has no answer to give
 * for it (`onLanded(null)`, from `409 KEY_COMMITTED`): the form has let go of its draft, so a
 * second press makes nothing, and the reader learns where the item will turn up. `what` names it
 * (`The issue`); `children` adds what this surface needs said beyond that.
 */
export function LandedWithoutAnswer({
  children,
  what,
}: {
  readonly children?: ReactNode;
  readonly what: string;
}): ReactNode {
  return (
    <p
      className={`text-sm ${textSecondaryOnSurface}`}
      data-testid="landed-without-answer"
      role="status"
    >
      {what} was made, but Dispatch lost its answer, so it may take a moment to appear.
      {children === undefined ? null : <> {children}</>}
    </p>
  );
}

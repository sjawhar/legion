import type { ReactNode } from "react";

import { borderDefault, card, skeletonBg } from "../theme/classes";

export function LoadingSkeleton({ label }: { label: string }): ReactNode {
  return (
    <div
      aria-busy="true"
      aria-label={label}
      className={`space-y-3 rounded-xl border p-4 ${card} ${borderDefault}`}
      role="status"
    >
      <div aria-hidden="true" className={`h-4 animate-pulse rounded ${skeletonBg}`} />
      <div aria-hidden="true" className={`h-4 animate-pulse rounded ${skeletonBg}`} />
      <div aria-hidden="true" className={`h-4 w-2/3 animate-pulse rounded ${skeletonBg}`} />
    </div>
  );
}

import type { ReactNode } from "react";

import { borderStrong, surfaceMutedBg, textMutedOnSurfaceMuted } from "../theme/classes";

export function EmptyState({
  action,
  label,
  message,
}: {
  action?: ReactNode;
  label: string;
  message: string;
}): ReactNode {
  return (
    <section
      aria-label={label}
      className={`rounded-xl border border-dashed px-4 py-6 text-center text-sm ${borderStrong} ${surfaceMutedBg} ${textMutedOnSurfaceMuted}`}
    >
      <p>{message}</p>
      {action === undefined ? null : <div className="mt-3">{action}</div>}
    </section>
  );
}

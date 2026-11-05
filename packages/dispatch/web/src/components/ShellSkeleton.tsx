import type { ReactNode } from "react";

import {
  canvasText,
  railActiveBg,
  railBg,
  railBorder,
  railText,
  skeletonBg,
} from "../theme/classes";

/**
 * The shell's frame before the shell exists: the rail, the compact header's title and three
 * placeholder rows, drawn from the entry chunk alone.
 *
 * It lives here rather than in `app.tsx` because two surfaces draw it and only one of them is
 * the app. The entry chunk shows it while the app's own chunk is still arriving
 * (`main.tsx`'s `Suspense`), and `AuthGate` shows it while the sign-in check is out
 * (`app.tsx`), so the frames before and after the app's chunk evaluates look the same and the
 * reader sees one skeleton, not two.
 */
export function ShellSkeleton(): ReactNode {
  return (
    <div aria-busy="true" className={`xl:flex ${canvasText}`}>
      <header
        className={`flex items-center px-3 xl:hidden ${railBorder} ${railBg} ${railText} border-b`}
      >
        <span className="text-lg font-semibold">Dispatch</span>
      </header>
      <aside
        className={`hidden p-5 xl:block xl:min-h-dvh xl:w-80 xl:border-r ${railBorder} ${railBg} ${railText}`}
      >
        <span className="text-lg font-semibold">Dispatch</span>
        <div aria-label="Loading navigation" className="mt-6 space-y-2" role="status">
          {[0, 1, 2, 3].map((row) => (
            <div className={`h-4 w-full animate-pulse rounded ${railActiveBg}`} key={row} />
          ))}
        </div>
      </aside>
      <main className="min-w-0 flex-1 p-6">
        <div aria-label="Loading Dispatch" className="space-y-3" role="status">
          {[0, 1, 2].map((row) => (
            <div className={`h-4 w-full animate-pulse rounded ${skeletonBg}`} key={row} />
          ))}
        </div>
      </main>
    </div>
  );
}

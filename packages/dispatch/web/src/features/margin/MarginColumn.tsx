import { lazy, type ReactNode, Suspense } from "react";

import type { MarginProps } from "./Margin";

/**
 * Where the shell places the margin, and where the margin's own code is fetched: after the first
 * paint, not before it.
 *
 * The margin is mounted on every route, and draws nothing on most of them - it returns null on a
 * desktop route that has no margin (`Margin.tsx`) - but it carries the comment composer, the
 * thread cards and the Markdown surfaces under them, a tenth of what the entry chunk used to
 * make every first page load parse. Nothing of it is on any route's first frame, so the shell
 * loads it the way `features/doc/editor.ts` loads the editor: behind `import()`, beside the
 * component it loads rather than in the shell, since the margin's rule for what it draws is the
 * margin's own.
 *
 * It stays mounted once it arrives, as it always was: the tab, the open thread and the composer
 * the reader left behind on an issue are still there when they come back to it
 * (`useMarginSheet.ts`). `fallback={null}` is what the reader sees on the frames before it
 * arrives, which is what the margin itself draws on the route they land on.
 */
const Margin = lazy(() => import("./Margin").then((module) => ({ default: module.Margin })));

export function MarginColumn(props: MarginProps): ReactNode {
  return (
    <Suspense fallback={null}>
      <Margin {...props} />
    </Suspense>
  );
}

import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react";
import { useLocation } from "react-router-dom";

import { parseIssuePath } from "../refs/routes";

/**
 * How a page marks the frame the shell draws around it: the column it renders into and the
 * margin beside it, which is one page as a reader reads it even though the shell mounts the
 * margin once for every route (`app.tsx`, `features/margin/margin-slot.tsx`).
 *
 * The issue page is the one page that needs it: its root is `[data-issue-page]`, and everything
 * the check reads inside that root - its identity block, its body and the panel's properties -
 * has to be inside one element (dispatch://INFRA-1196, `e2e/issue-page.ts`). The shell spreads
 * whatever the page registers and reads none of it, so it still knows nothing about which page
 * is showing.
 */
export type FrameAttributes = Readonly<Record<`data-${string}`, string>>;

/** What the page marked the frame with, read by the shell. */
const FrameContext = createContext<FrameAttributes | undefined>(undefined);

/** Where a page registers its marks. A page outside the shell registers with nobody. */
const FrameRegistry = createContext<(attributes: FrameAttributes | undefined) => void>(() => {});

export function PageFrameProvider({
  children,
}: {
  children: (attributes: FrameAttributes) => ReactNode;
}): ReactNode {
  const { pathname, search } = useLocation();
  const [attributes, setAttributes] = useState<FrameAttributes | undefined>(undefined);
  const register = useCallback((next: FrameAttributes | undefined) => setAttributes(next), []);
  // Until the page registers, the address answers: the issue page is behind a lazy chunk and a
  // whoami, so a reload would otherwise paint its column and its margin with no root around
  // them for a few frames, and a reader's own reload would land on a page that is not yet the
  // page (dispatch://INFRA-1196 R12). The shell still reads nothing the page marks; it reads
  // the address, which it already does for the margin (`routeHasMargin`).
  const marked = attributes ?? (parseIssuePath(pathname, search) === undefined ? {} : ISSUE_FRAME);
  return (
    <FrameRegistry.Provider value={register}>
      <FrameContext.Provider value={attributes}>{children(marked)}</FrameContext.Provider>
    </FrameRegistry.Provider>
  );
}

/** The issue page's own root, which every browser check reads the page through
 *  (`e2e/issue-page.ts`): the shell's frame around the page's column and the margin beside it. */
export const ISSUE_FRAME: FrameAttributes = { "data-issue-page": "" };

/**
 * Marks the shell's frame with `attributes` while the calling page is mounted. `attributes` must
 * be stable across renders: the frame holds it in state, so a fresh object every render would
 * register on every commit.
 */
export function usePageFrame(attributes: FrameAttributes): void {
  const register = useContext(FrameRegistry);
  useEffect(() => {
    register(attributes);
    return () => register(undefined);
  }, [attributes, register]);
}

import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react";

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
  const [attributes, setAttributes] = useState<FrameAttributes | undefined>(undefined);
  const register = useCallback((next: FrameAttributes | undefined) => setAttributes(next), []);
  return (
    <FrameRegistry.Provider value={register}>
      <FrameContext.Provider value={attributes}>{children(attributes ?? {})}</FrameContext.Provider>
    </FrameRegistry.Provider>
  );
}

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

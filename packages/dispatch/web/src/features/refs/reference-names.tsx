import { createContext, type ReactNode, useCallback, useContext, useEffect, useState } from "react";

/**
 * The names a page has already read for the references it draws, shared with every surface the
 * shell mounts beside it (dispatch://INFRA-1196 R20).
 *
 * A link to a block shows the block's own words, and reading them one link at a time is one
 * `GET /api/v1/references` per pointer - the read `GET /api/v1/reference-targets` exists to
 * replace ("the read a page that draws a chip per reference makes once instead of one
 * `GET /api/v1/references` per chip", `packages/contracts/AGENTS.md`). A page that has made that
 * batched read publishes its answer here, and every link under the shell reads its own name from
 * it instead of asking for it again. A page that publishes nothing leaves every link exactly as
 * it was, reading its own target.
 *
 * The store sits above the router, beside the margin's slot (`features/margin/margin-slot.tsx`),
 * because the margin is mounted beside the router and its cards draw the same references as the
 * page's own column: a context inside the page would not reach them.
 */
const ReferenceNamesContext = createContext<ReadonlyMap<string, string> | undefined>(undefined);

/** Where a page publishes what it read. A page outside the provider publishes to nobody. */
const ReferenceNamesRegistry = createContext<
  (names: ReadonlyMap<string, string> | undefined) => void
>(() => {});

export function ReferenceNamesProvider({ children }: { children: ReactNode }): ReactNode {
  const [names, setNames] = useState<ReadonlyMap<string, string> | undefined>(undefined);
  const register = useCallback(
    (next: ReadonlyMap<string, string> | undefined) => setNames(next),
    []
  );
  return (
    <ReferenceNamesRegistry.Provider value={register}>
      <ReferenceNamesContext.Provider value={names}>{children}</ReferenceNamesContext.Provider>
    </ReferenceNamesRegistry.Provider>
  );
}

/**
 * Publishes `names` for as long as the calling page is mounted, and empties the store again
 * when the reader leaves. `names` must be stable across renders (`useMemo`): the store holds it
 * in state, so a fresh map every render would register on every commit.
 */
export function usePublishReferenceNames(names: ReadonlyMap<string, string> | undefined): void {
  const register = useContext(ReferenceNamesRegistry);
  useEffect(() => {
    register(names);
    return () => register(undefined);
  }, [names, register]);
}

/** The name the page read for `reference`, or `undefined` where it read none and the caller
 *  must read its own. */
export function useKnownReferenceName(reference: string | undefined): string | undefined {
  const names = useContext(ReferenceNamesContext);
  return reference === undefined ? undefined : names?.get(reference);
}

import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ComposerAnchor, ComposerKind } from "../conversation/MentionComposer";
import { pulseBlock } from "../doc/marks";
import type { MarkPlacement } from "./useMarginItems";

/** What a retype answers: the new mark, or why nothing changed. The library's `RetypeOutcome`,
 *  restated here so the margin does not import the editor package. */
export type RetypeOutcome =
  | { readonly markId: string; readonly quote: string }
  | { readonly refused: "missing" | "unmarkable" | "overlaps" };

export interface DocumentBridge {
  focusBlock(blockId: string): void;
  focusMark(markId: string): void;
  /** Removes the record mark `markId` from the document, whatever its kind. */
  removeMark(markId: string): void;
  /** Replaces the provisional mark `markId` with one of `kind` over the same text. */
  retypeMark(markId: string, kind: ComposerKind): RetypeOutcome;
  setActiveBlocks(blockIds: readonly string[]): void;
  setActiveMarks(markIds: readonly string[]): void;
}

/** What the composer says when its kind cannot change, in the reader's words. */
const KIND_SWITCH_REFUSALS: Record<"missing" | "unmarkable" | "overlaps", string> = {
  missing:
    "That highlight is gone from the document. Close this composer and select the text again.",
  overlaps:
    "Someone else's comment already covers part of this text. Close this composer and select text outside it.",
  unmarkable:
    "A suggestion needs whole words inside one table cell. Comment or ask about this selection instead, or close this composer and select again.",
};

interface MarkComposeRequest {
  anchor: ComposerAnchor;
  kind: ComposerKind;
}

interface MarginContextValue {
  blockFilterId: string | undefined;
  blockFocusRequest: { blockId: string; seq: number } | undefined;
  blockPlacements: ReadonlyMap<string, MarkPlacement>;
  clearBlockFilter(): void;
  composeForMark(request: MarkComposeRequest): Promise<void>;
  documentBridge: DocumentBridge | undefined;
  filterToBlock(blockId: string): void;
  focusBlock(blockId: string): void;
  focusItemForMark(markId: string): void;
  focusRequest: { markId: string; seq: number } | undefined;
  hoverItemForMark(markId: string | null): void;
  hoveredItemId: string | undefined;
  hoveredMarkId: string | undefined;
  markPlacements: ReadonlyMap<string, MarkPlacement>;
  pendingCompose: (MarkComposeRequest & { seq: number }) | undefined;
  /** Whether the open document has reported its layout: it says so by publishing placements,
   *  and takes the answer back when it unregisters. An empty map is still an answer - a document
   *  with no live mark and no typed block has one - so the maps cannot stand in for this. */
  placementsReported: boolean;
  registerDocument(bridge: DocumentBridge | undefined): void;
  /** The open mark composer's kind switch: retypes its mark and moves the pending compose to the
   *  new mark, or answers why the switch was refused, for the composer to show. */
  retypeCompose(kind: ComposerKind): string | undefined;
  selectItem(id: string): void;
  selectedItemId: string | undefined;
  setBlockPlacements(placements: ReadonlyMap<string, MarkPlacement>): void;
  setHoveredItemId(id: string | undefined): void;
  setMarkItemIds(markItemIds: ReadonlyMap<string, string>): void;
  setMarkPlacements(placements: ReadonlyMap<string, MarkPlacement>): void;
  settleCompose(outcome: "saved" | "cancelled"): void;
}

const unavailableMargin = (): never => {
  throw new Error("MarginProvider is required");
};

const MarginContext = createContext<MarginContextValue>({
  blockFilterId: undefined,
  blockFocusRequest: undefined,
  blockPlacements: new Map(),
  clearBlockFilter: unavailableMargin,
  composeForMark: unavailableMargin,
  documentBridge: undefined,
  filterToBlock: unavailableMargin,
  focusBlock: unavailableMargin,
  focusItemForMark: unavailableMargin,
  focusRequest: undefined,
  hoverItemForMark: unavailableMargin,
  hoveredItemId: undefined,
  hoveredMarkId: undefined,
  markPlacements: new Map(),
  pendingCompose: undefined,
  placementsReported: false,
  registerDocument: unavailableMargin,
  retypeCompose: unavailableMargin,
  selectItem: unavailableMargin,
  selectedItemId: undefined,
  setBlockPlacements: unavailableMargin,
  setHoveredItemId: unavailableMargin,
  setMarkItemIds: unavailableMargin,
  setMarkPlacements: unavailableMargin,
  settleCompose: unavailableMargin,
});

export function MarginProvider({ children }: { children: ReactNode }): ReactNode {
  const [blockFilterId, setBlockFilterId] = useState<string>();
  const [blockFocusRequest, setBlockFocusRequest] = useState<{
    blockId: string;
    seq: number;
  }>();
  const [blockPlacements, setBlockPlacements] = useState<ReadonlyMap<string, MarkPlacement>>(
    () => new Map()
  );
  const [documentBridge, setDocumentBridge] = useState<DocumentBridge>();
  const [focusRequest, setFocusRequest] = useState<{ markId: string; seq: number }>();
  const [hoveredItemId, setHoveredItemId] = useState<string>();
  const [hoveredMarkId, setHoveredMarkId] = useState<string>();
  const [markPlacements, setMarkPlacements] = useState<ReadonlyMap<string, MarkPlacement>>(
    () => new Map()
  );
  const [placementsReported, setPlacementsReported] = useState(false);
  const [pendingCompose, setPendingCompose] = useState<
    (MarkComposeRequest & { seq: number }) | undefined
  >();
  const [selectedItemId, setSelectedItemId] = useState<string>();
  const markItemIds = useRef<ReadonlyMap<string, string>>(new Map());
  const sequence = useRef(0);
  const composePromise = useRef<{ reject(reason: Error): void; resolve(): void } | undefined>(
    undefined
  );
  // Mirrors of `documentBridge` and `pendingCompose` for the compose callbacks, which stay
  // stable (the editor holds `composeForMark` for the document's lifetime).
  const bridgeRef = useRef<DocumentBridge | undefined>(undefined);
  const pendingRef = useRef<(MarkComposeRequest & { seq: number }) | undefined>(undefined);

  // The margin owns the provisional mark a compose request names: it leaves the document when the
  // composer ends unsaved - cancelled, or replaced by a newer composer - and it changes kind with
  // the composer (`retypeCompose`). The editor's own catch (`runAction` in
  // @legion/proof-editor's dispatch-action-bar.ts) still removes the mark it created, which is a
  // no-op by then, and cannot know a retyped mark's id.
  const composeForMark = useCallback(
    (request: MarkComposeRequest): Promise<void> =>
      new Promise<void>((resolve, reject) => {
        const replaced = pendingRef.current;
        if (replaced !== undefined) {
          bridgeRef.current?.removeMark(replaced.anchor.mark_id);
        }
        composePromise.current?.reject(new Error("replaced by a newer composer"));
        composePromise.current = { reject, resolve };
        sequence.current += 1;
        const pending = { ...request, seq: sequence.current };
        pendingRef.current = pending;
        setPendingCompose(pending);
      }),
    []
  );
  const settleCompose = useCallback((outcome: "saved" | "cancelled") => {
    const pending = composePromise.current;
    const request = pendingRef.current;
    composePromise.current = undefined;
    pendingRef.current = undefined;
    setPendingCompose(undefined);
    if (outcome === "cancelled" && request !== undefined) {
      bridgeRef.current?.removeMark(request.anchor.mark_id);
    }
    if (pending === undefined) {
      return;
    }
    if (outcome === "saved") {
      pending.resolve();
      return;
    }
    pending.reject(new Error("composer closed"));
  }, []);
  const retypeCompose = useCallback((kind: ComposerKind): string | undefined => {
    const current = pendingRef.current;
    if (current === undefined || current.kind === kind) {
      return undefined;
    }
    // No open document means no mark to retype: the same words as a mark that is gone.
    const outcome = bridgeRef.current?.retypeMark(current.anchor.mark_id, kind) ?? {
      refused: "missing" as const,
    };
    if ("refused" in outcome) {
      return KIND_SWITCH_REFUSALS[outcome.refused];
    }
    const next = {
      ...current,
      anchor: { ...current.anchor, mark_id: outcome.markId, quote: outcome.quote },
      kind,
    };
    pendingRef.current = next;
    setPendingCompose(next);
    return undefined;
  }, []);
  const focusItemForMark = useCallback((markId: string) => {
    sequence.current += 1;
    setFocusRequest({ markId, seq: sequence.current });
  }, []);
  const focusBlock = useCallback(
    (blockId: string) => {
      sequence.current += 1;
      pulseBlock(blockId);
      setBlockFocusRequest({ blockId, seq: sequence.current });
      documentBridge?.focusBlock(blockId);
    },
    [documentBridge]
  );
  const hoverItemForMark = useCallback((markId: string | null) => {
    setHoveredMarkId(markId ?? undefined);
    setHoveredItemId(markId === null ? undefined : markItemIds.current.get(markId));
  }, []);
  const selectHoveredItem = useCallback((itemId: string | undefined) => {
    setHoveredMarkId(undefined);
    setHoveredItemId(itemId);
  }, []);
  const setMarkItemIds = useCallback((nextMarkItemIds: ReadonlyMap<string, string>) => {
    markItemIds.current = nextMarkItemIds;
  }, []);
  // Placements describe the open document. The provider outlives the route, so a document that
  // unregisters has to take its offsets with it: left behind, they place the next document's
  // cards from the last one's layout, and they tell the link's hold that this landing is already
  // over before the new document has reported anything.
  const registerDocument = useCallback((bridge: DocumentBridge | undefined) => {
    bridgeRef.current = bridge;
    setDocumentBridge(bridge);
    if (bridge === undefined) {
      setBlockPlacements(new Map());
      setMarkPlacements(new Map());
      setPlacementsReported(false);
    }
  }, []);
  const publishBlockPlacements = useCallback((placements: ReadonlyMap<string, MarkPlacement>) => {
    setBlockPlacements(placements);
    setPlacementsReported(true);
  }, []);
  const publishMarkPlacements = useCallback((placements: ReadonlyMap<string, MarkPlacement>) => {
    setMarkPlacements(placements);
    setPlacementsReported(true);
  }, []);
  const filterToBlock = useCallback((blockId: string) => {
    setBlockFilterId(blockId);
  }, []);
  const clearBlockFilter = useCallback(() => {
    setBlockFilterId(undefined);
  }, []);
  const value = useMemo<MarginContextValue>(
    () => ({
      blockFilterId,
      blockFocusRequest,
      blockPlacements,
      clearBlockFilter,
      composeForMark,
      documentBridge,
      filterToBlock,
      focusBlock,
      focusItemForMark,
      focusRequest,
      hoverItemForMark,
      hoveredItemId,
      hoveredMarkId,
      markPlacements,
      pendingCompose,
      placementsReported,
      registerDocument,
      retypeCompose,
      selectItem: setSelectedItemId,
      selectedItemId,
      setBlockPlacements: publishBlockPlacements,
      setHoveredItemId: selectHoveredItem,
      setMarkItemIds,
      setMarkPlacements: publishMarkPlacements,
      settleCompose,
    }),
    [
      blockFilterId,
      blockFocusRequest,
      blockPlacements,
      clearBlockFilter,
      composeForMark,
      documentBridge,
      filterToBlock,
      focusBlock,
      focusItemForMark,
      focusRequest,
      hoverItemForMark,
      hoveredItemId,
      hoveredMarkId,
      markPlacements,
      pendingCompose,
      placementsReported,
      publishBlockPlacements,
      publishMarkPlacements,
      registerDocument,
      retypeCompose,
      selectHoveredItem,
      selectedItemId,
      setMarkItemIds,
      settleCompose,
    ]
  );

  return <MarginContext.Provider value={value}>{children}</MarginContext.Provider>;
}

export function useMargin(): MarginContextValue {
  return useContext(MarginContext);
}

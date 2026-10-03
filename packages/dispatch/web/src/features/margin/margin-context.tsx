import { type MutationKey, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ComposerAnchor, ComposerKind } from "../conversation/composer-model";
import type { RetypeOutcome, RetypeRefusal } from "../doc/editor";
import { pulseBlock } from "../doc/marks";
import type { MarkPlacement } from "./useMarginItems";

export interface DocumentBridge {
  focusBlock(blockId: string): void;
  focusMark(markId: string): void;
  /** Removes the record mark `markId` from the document, whatever its kind. */
  removeMark(markId: string): void;
  /** Replaces the provisional mark `markId` with one of `kind` over the same text. */
  retypeMark(markId: string, kind: ComposerKind): RetypeOutcome;
  /** Names the mark the open composer holds, or null when none is open, so the document treats a
   *  selection-bar action that cuts into it as the composer's own write. */
  setComposerMark(markId: string | null): void;
  setActiveBlocks(blockIds: readonly string[]): void;
  setActiveMarks(markIds: readonly string[]): void;
}

/** What the composer says when its kind cannot change, in the reader's words. */
const KIND_SWITCH_REFUSALS: Record<RetypeRefusal["refused"], string> = {
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

/** `turnedAway`: a newer selection-bar action had to wait for this compose's send, and the margin
 *  brought this compose back on screen to say so. */
type PendingCompose = MarkComposeRequest & { seq: number; turnedAway?: true };

/** The name every margin compose's send sits beneath (`MentionComposer`'s `mutationKey`). */
export const MARGIN_COMPOSER_SEND_KEY: MutationKey = ["margin-composer"];

/** The name of the compose `seq`'s send. While it is out the open compose is that send's: it
 *  neither moves to a newer selection nor ends unsaved, so the send's outcome - its refusal, with
 *  its draft - lands on the composer that sent it. A compose whose document the reader left with
 *  its send out stops being the open one, so its send holds no compose on the next document; it is
 *  the open one again once the reader is back on its own (`resumeCompose`). */
export function marginComposeSendKey(seq: number): MutationKey {
  return [...MARGIN_COMPOSER_SEND_KEY, seq];
}

/** The open mark composer: what it is about, and the editor's promise it settles. */
interface OpenCompose {
  reject(reason: Error): void;
  request: PendingCompose;
  resolve(): void;
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
  pendingCompose: PendingCompose | undefined;
  /** Whether the open document has reported its layout: it says so by publishing placements,
   *  and takes the answer back when it unregisters. An empty map is still an answer - a document
   *  with no live mark and no typed block has one - so the maps cannot stand in for this. */
  placementsReported: boolean;
  registerDocument(bridge: DocumentBridge | undefined): void;
  /** The open mark composer's kind switch: retypes its mark and moves the pending compose to the
   *  new mark, or answers why the switch was refused, for the composer to show. */
  retypeCompose(kind: ComposerKind): string | undefined;
  /** Makes a compose the reader left with its send out (`settleCompose`'s `left`) the open one
   *  again, now that they are back on its document - unless a compose is open already. Its
   *  editor's promise was settled when they left, so nothing settles it now. */
  resumeCompose(compose: MarkComposeRequest & { seq: number }): void;
  selectItem(id: string): void;
  selectedItemId: string | undefined;
  setBlockPlacements(placements: ReadonlyMap<string, MarkPlacement>): void;
  setHoveredItemId(id: string | undefined): void;
  setMarkItemIds(markItemIds: ReadonlyMap<string, string>): void;
  setMarkPlacements(placements: ReadonlyMap<string, MarkPlacement>): void;
  /** Settles the compose `seq` - the one a composer's own send or close names - if it is still
   *  the open one: a send a compose left behind lands after a newer compose opened, and settles
   *  nothing. `left`: the reader left its document while its send was out. It stops being the
   *  open compose and its mark stays, since the send names it; the editor that wrote the mark is
   *  gone with the document, so its promise resolves. */
  settleCompose(outcome: "saved" | "cancelled" | "left", seq: number): void;
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
  resumeCompose: unavailableMargin,
  selectItem: unavailableMargin,
  selectedItemId: undefined,
  setBlockPlacements: unavailableMargin,
  setHoveredItemId: unavailableMargin,
  setMarkItemIds: unavailableMargin,
  setMarkPlacements: unavailableMargin,
  settleCompose: unavailableMargin,
});

export function MarginProvider({ children }: { children: ReactNode }): ReactNode {
  const queryClient = useQueryClient();
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
  const [pendingCompose, setPendingCompose] = useState<PendingCompose>();
  const [selectedItemId, setSelectedItemId] = useState<string>();
  const markItemIds = useRef<ReadonlyMap<string, string>>(new Map());
  const sequence = useRef(0);
  // Mirrors of the open compose and `documentBridge` for the compose callbacks, which stay stable
  // (the editor holds `composeForMark` for the document's lifetime).
  const openCompose = useRef<OpenCompose | undefined>(undefined);
  const bridgeRef = useRef<DocumentBridge | undefined>(undefined);
  // Every change to the open compose goes through here: the ref the callbacks read, the state the
  // sheet renders, and the mark the document treats as the composer's own.
  const publishCompose = useCallback((next: OpenCompose | undefined) => {
    openCompose.current = next;
    setPendingCompose(next?.request);
    bridgeRef.current?.setComposerMark(next?.request.anchor.mark_id ?? null);
  }, []);

  // The margin owns the provisional mark a compose request names: it leaves the document when the
  // composer ends unsaved - cancelled, or replaced by a newer composer - and it changes kind with
  // the composer (`retypeCompose`). The editor's own catch (`runAction` in
  // @legion/proof-editor's dispatch-action-bar.ts) still removes the mark it created, which is a
  // no-op by then, and cannot know a retyped mark's id. A newer selection-bar action while the
  // open compose's own send is out is refused, and the margin takes back the mark that action
  // wrote, as it does a replaced one - that catch cannot see a provisional comment, whose body is
  // empty (`removeRecordMark` in @legion/proof-editor says why). The compose is the send's until
  // the server answers: re-pointing it would hand the send's outcome to the newer one - a success
  // closing it as saved, a refusal's Retry posting to its mark. The refused action shows the
  // reader why nothing opened: the open compose is published again, `turnedAway`, so the margin
  // brings it back on screen saying its send is still out.
  const composeForMark = useCallback(
    (request: MarkComposeRequest): Promise<void> =>
      new Promise<void>((resolve, reject) => {
        const open = openCompose.current;
        if (
          open !== undefined &&
          queryClient.isMutating({ mutationKey: marginComposeSendKey(open.request.seq) }) > 0
        ) {
          bridgeRef.current?.removeMark(request.anchor.mark_id);
          publishCompose({ ...open, request: { ...open.request, turnedAway: true } });
          reject(new Error("the open composer's send is out"));
          return;
        }
        if (open !== undefined) {
          bridgeRef.current?.removeMark(open.request.anchor.mark_id);
          open.reject(new Error("replaced by a newer composer"));
        }
        sequence.current += 1;
        publishCompose({ reject, request: { ...request, seq: sequence.current }, resolve });
      }),
    [publishCompose, queryClient]
  );
  const settleCompose = useCallback(
    (outcome: "saved" | "cancelled" | "left", seq: number) => {
      const open = openCompose.current;
      // Nothing to settle once the compose closed - the composer closing after its save - or when
      // a newer compose is the open one.
      if (open === undefined || open.request.seq !== seq) {
        return;
      }
      if (outcome === "cancelled") {
        bridgeRef.current?.removeMark(open.request.anchor.mark_id);
      }
      publishCompose(undefined);
      if (outcome === "cancelled") {
        open.reject(new Error("composer closed"));
        return;
      }
      open.resolve();
    },
    [publishCompose]
  );
  const resumeCompose = useCallback(
    ({ anchor, kind, seq }: MarkComposeRequest & { seq: number }) => {
      if (openCompose.current !== undefined) return;
      // Its editor's promise was settled when the reader left its document: nothing to settle.
      publishCompose({ reject: () => {}, request: { anchor, kind, seq }, resolve: () => {} });
    },
    [publishCompose]
  );
  const retypeCompose = useCallback(
    (kind: ComposerKind): string | undefined => {
      const open = openCompose.current;
      if (open === undefined || open.request.kind === kind) {
        return undefined;
      }
      // No open document means no mark to retype: the same words as a mark that is gone.
      const outcome = bridgeRef.current?.retypeMark(open.request.anchor.mark_id, kind) ?? {
        refused: "missing" as const,
      };
      if ("refused" in outcome) {
        return KIND_SWITCH_REFUSALS[outcome.refused];
      }
      publishCompose({
        ...open,
        request: {
          ...open.request,
          anchor: { ...open.request.anchor, mark_id: outcome.markId, quote: outcome.quote },
          kind,
        },
      });
      return undefined;
    },
    [publishCompose]
  );
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
    // A document registers fresh whenever `ProofDocument` remounts its editor - a new artifact or
    // block schema - and the margin's open composer outlives that: the new editor has to learn
    // which mark the composer holds, as `publishCompose` tells the one it replaces.
    bridge?.setComposerMark(openCompose.current?.request.anchor.mark_id ?? null);
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
      resumeCompose,
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
      resumeCompose,
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

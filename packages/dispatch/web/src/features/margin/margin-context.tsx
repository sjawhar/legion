import { type MutationKey, partialMatchKey, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { isAnchorMissing } from "../../api/client";
import type { AnchorRefusal, ComposerAnchor, ComposerKind } from "../conversation/composer-model";
import type { HeldSend } from "../conversation/held-sends";
import { heldSends } from "../conversation/held-sends";
import type { RemarkOutcome, RetypeOutcome, RetypeRefusal } from "../doc/editor";
import type { MarkPlacement } from "./useMarginItems";

export interface DocumentBridge {
  /** The artifact the registered document shows. The link hold compares it with the artifact
   *  whose cards the margin shows before it trusts the registered document's layout report. */
  artifactId: string;
  focusBlock(blockId: string): void;
  focusMark(markId: string): void;
  /** Removes the record mark `markId` from the document, whatever its kind. */
  removeMark(markId: string): void;
  /** Writes the provisional mark `markId` again, as one of `kind` under a fresh id, over `quote` -
   *  the words the refused send is anchored to - wherever they read now, so a send the server
   *  refused for a mark it cannot see has something to anchor to. The words travel with the
   *  request because the editor may have kept nothing: another writer's change reaches it as a
   *  rebuilt document, and a reconnect rebuilds the editor. */
  remarkMark(markId: string, kind: ComposerKind, quote: string): RemarkOutcome;
  /** Replaces the provisional mark `markId` with one of `kind` over the same text. */
  retypeMark(markId: string, kind: ComposerKind): RetypeOutcome;
  setActiveBlocks(blockIds: readonly string[]): void;
  setActiveMarks(markIds: readonly string[]): void;
  /** Whether this browser's edits of the document have all reached the server, within
   *  `timeoutMs`: a mark written here is the server's to see before a send names it. */
  whenSent(timeoutMs: number): Promise<boolean>;
}

/** What the composer says when its kind cannot change, in the reader's words. */
const KIND_SWITCH_REFUSALS: Record<RetypeRefusal["refused"], string> = {
  missing:
    "That highlight is gone from the document. Close this composer and select the text again.",
  unmarkable:
    "A suggestion needs whole words inside one table cell. Comment or ask about this selection instead, or close this composer and select again.",
};

/** What the composer says in place of the server's `ANCHOR_MISSING` words when the margin could
 *  not write the mark again: the words the send is anchored to read nowhere in the document, or
 *  they read in more than one place and nothing the editor kept says which one the reader
 *  selected - either way only a fresh selection can give the draft an anchor, which is what this
 *  says and the only thing it says. The server's own message is unchanged - agents' clients read
 *  it - and this one is the reader's. */
const REANCHOR_REFUSAL =
  "The text you selected was changed or deleted, so this comment has nothing to attach to. Your draft is kept — select the text again to attach it.";

/** How long the margin waits for the mark it wrote again to reach the server before it sends the
 *  draft again. The document's pending count holds every tab's rows, and this page cannot make
 *  the server take another tab's, so the wait always ends and the retry goes either way. */
const REANCHOR_SEND_WAIT_MS = 5000;

interface MarkComposeRequest {
  anchor: ComposerAnchor;
  kind: ComposerKind;
}

type PendingCompose = MarkComposeRequest & { seq: number };

/** A compose the margin shows the reader: a new one, or one whose document a newer selection-bar
 *  action had to wait on (`turnedAway`), or one held for the document the reader came back to.
 *  `seq` tells two showings of one document apart. */
export interface ShownCompose {
  artifact: string;
  seq: number;
  turnedAway: boolean;
}

/** The name every margin compose's send sits beneath (`MentionComposer`'s `mutationKey`). */
export const MARGIN_COMPOSER_SEND_KEY: MutationKey = ["margin-composer"];

/** The name of the send of the compose on the document `artifactId`, at most one per document.
 *  The held-send store holds it - its draft, its refusal and the mark it names - until it lands
 *  or the reader discards it, whatever the reader does meanwhile: while it is out, a newer
 *  selection-bar action on that document waits on it. */
export function marginComposeSendKey(artifactId: string): MutationKey {
  return [...MARGIN_COMPOSER_SEND_KEY, artifactId];
}

/** The name every margin thread's reply send sits beneath. */
export const MARGIN_REPLY_SEND_KEY: MutationKey = ["margin-thread-reply"];

/** The name of the reply send of the margin thread `key`: the store holds it for the thread, and
 *  a phone thread's Back and Escape hold on it while it is out. */
export function marginReplySendKey(key: string): MutationKey {
  return [...MARGIN_REPLY_SEND_KEY, key];
}

/** The open mark composer: what it is about, and the editor's promise it settles. */
interface OpenCompose {
  reject(reason: Error): void;
  request: PendingCompose;
  resolve(): void;
}

interface MarginContextValue {
  /** Why the compose's mark could not be written again after the server refused its send for a
   *  mark it cannot see, in the reader's words, kept with the mark it answers: the composer shows
   *  it in place of the server's words, and a newer anchor leaves it behind. */
  anchorRefusal: AnchorRefusal | undefined;
  blockFilterId: string | undefined;
  blockPlacements: ReadonlyMap<string, MarkPlacement>;
  /** Ends the open compose `seq` unsaved, if it is still the open one: its mark leaves the
   *  document and the editor's promise rejects. */
  cancelCompose(seq: number): void;
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
  /** The compose the reader has open and has not sent. */
  pendingCompose: PendingCompose | undefined;
  /** Whether the registered document has reported its layout: it says so by publishing
   *  placements, and the answer resets whenever a document registers or unregisters, or the
   *  registered document withdraws a hidden layout. An empty map is still an answer - a document
   *  with no live mark and no typed block has one - so the maps cannot stand in for this. */
  placementsReported: boolean;
  registerDocument(bridge: DocumentBridge | undefined): void;
  /** Shows the compose held for `artifact`, which the reader just came back to. */
  revealCompose(artifact: string): void;
  /** The kind switch of the composer on the open document: retypes its mark and moves the compose
   *  - the open one, or a refused one the store holds - to the new mark, or answers why the switch
   *  was refused, for the composer to show. */
  retypeCompose(kind: ComposerKind): string | undefined;
  selectItem(id: string): void;
  selectedItemId: string | undefined;
  setBlockPlacements(placements: ReadonlyMap<string, MarkPlacement>): void;
  setHoveredItemId(id: string | undefined): void;
  setMarkItemIds(markItemIds: ReadonlyMap<string, string>): void;
  setMarkPlacements(placements: ReadonlyMap<string, MarkPlacement>): void;
  shownCompose: ShownCompose | undefined;
  /** Takes back the registered document's layout while it is hidden and has no layout to report. */
  withdrawPlacements(): void;
}

const unavailableMargin = (): never => {
  throw new Error("MarginProvider is required");
};

const MarginContext = createContext<MarginContextValue>({
  anchorRefusal: undefined,
  blockFilterId: undefined,
  blockPlacements: new Map(),
  cancelCompose: unavailableMargin,
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
  revealCompose: unavailableMargin,
  retypeCompose: unavailableMargin,
  selectItem: unavailableMargin,
  selectedItemId: undefined,
  setBlockPlacements: unavailableMargin,
  setHoveredItemId: unavailableMargin,
  setMarkItemIds: unavailableMargin,
  setMarkPlacements: unavailableMargin,
  shownCompose: undefined,
  withdrawPlacements: unavailableMargin,
});

export function MarginProvider({ children }: { children: ReactNode }): ReactNode {
  const queryClient = useQueryClient();
  const store = heldSends(queryClient);
  const [blockFilterId, setBlockFilterId] = useState<string>();
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
  const [shownCompose, setShownCompose] = useState<ShownCompose>();
  const [selectedItemId, setSelectedItemId] = useState<string>();
  const [anchorRefusal, setAnchorRefusal] = useState<AnchorRefusal>();
  const markItemIds = useRef<ReadonlyMap<string, string>>(new Map());
  const sequence = useRef(0);
  // The marks the margin wrote again itself. A send the server refuses for one of them is the
  // reader's to see as the server put it: the recovery has already run once for this draft, and
  // running it again would send the same words to the same refusal.
  const remarked = useRef(new Set<string>());
  // Mirrors of the open compose and `documentBridge` for the compose callbacks, which stay stable
  // (the editor holds `composeForMark` for the document's lifetime).
  const openCompose = useRef<OpenCompose | undefined>(undefined);
  const bridgeRef = useRef<DocumentBridge | undefined>(undefined);
  const show = useCallback((artifact: string, turnedAway: boolean) => {
    sequence.current += 1;
    setShownCompose({ artifact, seq: sequence.current, turnedAway });
  }, []);
  // Every change to the open compose goes through here: the ref the callbacks read, and the state
  // the sheet renders.
  const publishCompose = useCallback((next: OpenCompose | undefined) => {
    openCompose.current = next;
    setPendingCompose(next?.request);
  }, []);
  // The server refuses a send whose mark the document no longer holds (`409 ANCHOR_MISSING`):
  // the reader held the composer open while another writer rewrote the sentence, or their tab was
  // offline past the provisional mark's time, or the send reached a server process that cannot
  // see this browser at all. The margin writes the mark again over the same words wherever they
  // are now, re-addresses the held send to it, waits for this browser's edits to reach the server
  // so the mark the retry names is one the server can see, and sends it as a new press: a fresh
  // `Idempotency-Key`, never a second body under the key Dispatch has already answered, which it
  // would answer with that same 409 or refuse outright (`request_keys.go`). The draft stays where
  // it is throughout, as it does for every refusal.
  const reanchor = useCallback(
    async (held: HeldSend & { status: "refused" }): Promise<void> => {
      const anchor = held.request.anchor;
      if (anchor === undefined) return;
      const bridge = bridgeRef.current;
      // The compose's own document is not on screen, so there is no mark to write again: as far
      // as this page can tell, the words the reader selected are gone.
      if (bridge === undefined || bridge.artifactId !== anchor.artifact) {
        setAnchorRefusal({ markId: anchor.mark_id, text: REANCHOR_REFUSAL });
        return;
      }
      // The quote the refused send itself carries: the words the composer was opened over, which
      // outlive both the mark and the editor state that wrote it.
      const outcome = bridge.remarkMark(anchor.mark_id, held.request.kind, anchor.quote);
      if ("refused" in outcome) {
        setAnchorRefusal({ markId: anchor.mark_id, text: REANCHOR_REFUSAL });
        return;
      }
      remarked.current.add(outcome.markId);
      setAnchorRefusal(undefined);
      store.readdress(held.mutationKey, {
        ...held.request,
        anchor: { ...anchor, mark_id: outcome.markId, quote: outcome.quote },
      });
      await bridge.whenSent(REANCHOR_SEND_WAIT_MS);
      store.retry(held.mutationKey);
    },
    [store]
  );
  // From Send on, the compose is its send's: the held-send store keeps its draft, its refusal and
  // the mark it names, whatever unmounts its composer, until the send lands or the reader discards
  // the refusal. So the open compose ends there - the editor's promise resolves, keeping the mark -
  // and a refusal the reader drops takes the mark out of the document.
  useEffect(() => {
    const unsubscribe = store.subscribe(() => {
      const open = openCompose.current;
      if (open !== undefined && store.get(marginComposeSendKey(open.request.anchor.artifact))) {
        publishCompose(undefined);
        open.resolve();
      }
    });
    const unlisten = store.onOutcome(({ kind, send }) => {
      const markId = send.request.anchor?.mark_id;
      if (markId === undefined || !partialMatchKey(send.mutationKey, MARGIN_COMPOSER_SEND_KEY)) {
        return;
      }
      if (kind !== "refused") {
        remarked.current.delete(markId);
        setAnchorRefusal((shown) => (shown?.markId === markId ? undefined : shown));
        if (kind === "discarded") bridgeRef.current?.removeMark(markId);
        return;
      }
      if (
        send.status === "refused" &&
        isAnchorMissing(send.refusal) &&
        !remarked.current.has(markId)
      ) {
        void reanchor(send);
      }
    });
    return () => {
      unsubscribe();
      unlisten();
    };
  }, [publishCompose, reanchor, store]);

  // The margin owns the provisional mark a compose request names: it leaves the document when the
  // composer ends unsaved - cancelled, or replaced by a newer composer - and it changes kind with
  // the composer (`retypeCompose`). The editor's own catch (`runAction` in
  // @legion/proof-editor's dispatch-action-bar.ts) still removes the mark it created, which is a
  // no-op by then, and cannot know a retyped mark's id. A newer selection-bar action on a document
  // whose compose's send is out is refused, and the margin takes back the mark that action wrote,
  // as it does a replaced one - that catch cannot see a provisional comment, whose body is empty
  // (`removeRecordMark` in @legion/proof-editor says why). The send's outcome is the compose's:
  // moving it to the newer selection would hand that outcome to the newer one - a success closing
  // it, a refusal's Retry posting to its mark. The refused action shows the reader why nothing
  // opened: the margin brings that compose on screen saying its send is still out. A refused send
  // gives way: the newer compose takes the composer the reader has there, which keeps its draft,
  // and the refusal and its mark go.
  const composeForMark = useCallback(
    (request: MarkComposeRequest): Promise<void> =>
      new Promise<void>((resolve, reject) => {
        const { artifact, mark_id: markId } = request.anchor;
        const held = store.get(marginComposeSendKey(artifact));
        if (held !== undefined && held.status !== "refused") {
          bridgeRef.current?.removeMark(markId);
          show(artifact, true);
          reject(new Error("the open composer's send is out"));
          return;
        }
        if (held !== undefined) store.discard(held.mutationKey);
        const open = openCompose.current;
        if (open !== undefined) {
          bridgeRef.current?.removeMark(open.request.anchor.mark_id);
          open.reject(new Error("replaced by a newer composer"));
        }
        sequence.current += 1;
        publishCompose({ reject, request: { ...request, seq: sequence.current }, resolve });
        show(artifact, false);
      }),
    [publishCompose, show, store]
  );
  const cancelCompose = useCallback(
    (seq: number) => {
      const open = openCompose.current;
      if (open === undefined || open.request.seq !== seq) return;
      bridgeRef.current?.removeMark(open.request.anchor.mark_id);
      publishCompose(undefined);
      open.reject(new Error("composer closed"));
    },
    [publishCompose]
  );
  const revealCompose = useCallback((artifact: string) => show(artifact, false), [show]);
  const retypeCompose = useCallback(
    (kind: ComposerKind): string | undefined => {
      const open = openCompose.current;
      const bridge = bridgeRef.current;
      const held =
        open === undefined && bridge !== undefined
          ? store.get(marginComposeSendKey(bridge.artifactId))
          : undefined;
      const anchor = open?.request.anchor ?? held?.request.anchor;
      const current = open?.request.kind ?? held?.request.kind;
      if (anchor === undefined || current === kind) return undefined;
      // No open document means no mark to retype: the same words as a mark that is gone.
      const outcome = bridge?.retypeMark(anchor.mark_id, kind) ?? { refused: "missing" as const };
      if ("refused" in outcome) {
        return KIND_SWITCH_REFUSALS[outcome.refused];
      }
      const retyped = { ...anchor, mark_id: outcome.markId, quote: outcome.quote };
      if (open !== undefined) {
        publishCompose({ ...open, request: { ...open.request, anchor: retyped, kind } });
      } else if (held !== undefined) {
        store.readdress(held.mutationKey, { ...held.request, anchor: retyped, kind });
      }
      return undefined;
    },
    [publishCompose, store]
  );
  const focusItemForMark = useCallback((markId: string) => {
    sequence.current += 1;
    setFocusRequest({ markId, seq: sequence.current });
  }, []);
  const focusBlock = useCallback(
    (blockId: string) => {
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
  const withdrawPlacements = useCallback(() => {
    setBlockPlacements(new Map());
    setMarkPlacements(new Map());
    setPlacementsReported(false);
  }, []);
  // Placements describe the registered document, so each registration - the next document, or
  // none - starts with no report. The provider outlives the route: offsets left behind would
  // place the next document's cards from the last one's layout, and would tell the link's hold
  // that this landing is already over before the new document has reported anything.
  const registerDocument = useCallback(
    (bridge: DocumentBridge | undefined) => {
      bridgeRef.current = bridge;
      setDocumentBridge(bridge);
      withdrawPlacements();
    },
    [withdrawPlacements]
  );
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
      anchorRefusal,
      blockFilterId,
      blockPlacements,
      cancelCompose,
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
      revealCompose,
      retypeCompose,
      selectItem: setSelectedItemId,
      selectedItemId,
      setBlockPlacements: publishBlockPlacements,
      setHoveredItemId: selectHoveredItem,
      setMarkItemIds,
      setMarkPlacements: publishMarkPlacements,
      shownCompose,
      withdrawPlacements,
    }),
    [
      anchorRefusal,
      blockFilterId,
      blockPlacements,
      cancelCompose,
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
      revealCompose,
      retypeCompose,
      selectHoveredItem,
      selectedItemId,
      setMarkItemIds,
      shownCompose,
      withdrawPlacements,
    ]
  );

  return <MarginContext.Provider value={value}>{children}</MarginContext.Provider>;
}

export function useMargin(): MarginContextValue {
  return useContext(MarginContext);
}

import { hashKey, type MutationKey, partialMatchKey, useQueryClient } from "@tanstack/react-query";
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
import type {
  RecordMarkPlacement,
  RemarkOutcome,
  RemarkRefusal,
  RetypedMark,
  RetypeOutcome,
  RetypeRefusal,
} from "../doc/editor";
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
   *  request, and `placement` - the block the editor wrote the mark in, the text it read on each
   *  side of those words and how many readings of them that block held - with them, because the
   *  editor may have kept nothing: another writer's change reaches it as a rebuilt document, and
   *  a reconnect rebuilds the editor. */
  remarkMark(
    markId: string,
    kind: ComposerKind,
    quote: string,
    placement: RecordMarkPlacement | undefined
  ): RemarkOutcome;
  /** Replaces the provisional mark `markId` with one of `kind` over the same text. */
  retypeMark(markId: string, kind: ComposerKind): RetypeOutcome;
  setActiveBlocks(blockIds: readonly string[]): void;
  setActiveMarks(markIds: readonly string[]): void;
  /** Answers once this browser's edits of the document have reached the server, and at
   *  `timeoutMs` whatever it reports: a mark written here is the server's to see before a send
   *  names it, and the caller sends either way. */
  whenSent(timeoutMs: number): Promise<void>;
}

/** A composer that holds the draft of a held send, as `MentionComposer` does: it writes what the
 *  reader has typed into the store a short wait after each keystroke, and the margin asks it to
 *  write it now before it sends that send again itself (`reanchor`). */
export interface ComposerBridge {
  /** Writes what the reader has typed into the held send's draft, as the composer's own
   *  debounced write does. */
  keepDraft(): void;
  /** The held send this composer's draft belongs to (`MentionComposer`'s `mutationKey`). */
  mutationKey: MutationKey;
}

/** The anchor `anchor` re-addressed to `mark`, the mark the editor has just written in place of
 *  the one it names: the only place the two meet, so a field `RetypedMark` gains reaches both
 *  the margin's re-anchor and the composer's kind switch. */
export function anchorAt(anchor: ComposerAnchor, mark: RetypedMark): ComposerAnchor {
  return {
    ...anchor,
    mark_id: mark.markId,
    // The composer keeps no placement where the editor had none to give.
    placement: mark.placement ?? undefined,
    quote: mark.quote,
  };
}

/** What the composer says when its kind cannot change, in the reader's words. */
const KIND_SWITCH_REFUSALS: Record<RetypeRefusal["refused"], string> = {
  missing:
    "That highlight is gone from the document. Close this composer and select the text again.",
  unmarkable:
    "A suggestion needs whole words inside one table cell. Comment or ask about this selection instead, or close this composer and select again.",
};

/** The tail every re-anchor refusal ends on: whichever of them the reader meets, a fresh
 *  selection is the way out of it, and the draft is waiting where they left it. */
const DRAFT_KEPT = "Your draft is kept — select the text again to attach it.";

/** What the composer says in place of the server's `ANCHOR_MISSING` words when the margin could
 *  not write the mark again: what became of the words the send is anchored to, about the thing
 *  the reader is writing. A `ComposerKind` reads as its own word - comment, ask, suggestion -
 *  and this is the one place that puts it in a sentence. The six cases are six different
 *  things to be told: the words read nowhere in the document; the block the reader selected in
 *  still reads them, but the text around them changed, so nothing says which reading was theirs;
 *  they read in two places with the same text around both - two readings of that block, or two
 *  of the document once that block is gone - so the text they read beside their words reads
 *  beside both and nothing kept of the selection says which of the two the draft is about;
 *  they read in more than one place and the composer kept nothing of where they were selected,
 *  so there is nothing to tell those readings apart by; they read somewhere else, but not in
 *  the block the reader selected in; or they cannot carry a suggestion's mark at all. Only
 *  `"unplaceable"` says the words are no longer where they were selected, because only there
 *  is that true. The server's own message is unchanged - agents' clients read it - and this
 *  one is the reader's. */
export const REANCHOR_REFUSALS: Record<RemarkRefusal["refused"], (kind: ComposerKind) => string> = {
  ambiguous: (kind) =>
    `The words you selected read in more than one place with the same text around them, so nothing says which one this ${kind} is about. ${DRAFT_KEPT}`,
  gone: (kind) =>
    `The text you selected was changed or deleted, so this ${kind} has nothing to attach to. ${DRAFT_KEPT}`,
  unmarkable: (kind) =>
    `A suggestion needs whole words inside one table cell, so this ${kind} cannot be marked over the text you selected. ${DRAFT_KEPT}`,
  unplaceable: (kind) =>
    `The text you selected is still in the document, but no longer where you selected it, so this ${kind} cannot be attached on its own. ${DRAFT_KEPT}`,
  unremembered: (kind) =>
    `The words you selected read in more than one place, and nothing kept of where you selected them says which one this ${kind} is about. ${DRAFT_KEPT}`,
  unsurrounded: (kind) =>
    `The text around the words you selected changed, so nothing is left to say which reading of them this ${kind} is about. ${DRAFT_KEPT}`,
};

/** What the composer says when the document the refused send is anchored to is not the one on
 *  screen: no editor here holds that document, so nothing about the words can be read at all,
 *  and nothing says they were touched. Opening the document is the way out of this one rather
 *  than a fresh selection - the draft and its mark are waiting for that document - so it keeps
 *  the promise `DRAFT_KEPT` makes in the words that fit it. */
export const DOCUMENT_CLOSED_REFUSAL = (kind: ComposerKind): string =>
  `The document you selected the text in is not open, so there is nothing here to attach this ${kind} to. Your draft is kept — open that document and send it again to attach it.`;

/** What the composer says while the margin is still putting the mark back: a send resumed on a
 *  reload can be refused before the reader's own document has bound its editor, and the margin
 *  waits for that (`whenBridgeBinds`) rather than refusing at once. The server's own words for
 *  that refusal - "anchor mark is not in the document" - are about a mark the reader never saw,
 *  so this says what is happening to the thing they wrote instead. It asks nothing of them: the
 *  send goes again by itself once the mark is written. */
export const REANCHOR_IN_PROGRESS = (kind: ComposerKind): string =>
  `Re-attaching this ${kind} to the text you selected, and sending it again…`;

/** How long the margin waits for the mark it wrote again to reach the server before it sends the
 *  draft again. The document's pending count holds every tab's rows, and this page cannot make
 *  the server take another tab's, so the wait always ends and the retry goes either way. */
const REANCHOR_SEND_WAIT_MS = 5000;

/** How long a refused send waits for a document to register its bridge before the margin tells
 *  the reader the document is not open. The bridge binds only once the document has synced and
 *  its editor is built (`ProofDocument`'s `onSynced`, `createEditor`, `bindEditor`), while a
 *  reload resumes a margin send pressed within `KEPT_SEND_RESUME_MS`: the refusal of a send on
 *  the document the reader is looking at can land seconds before that document's editor exists.
 *  Long enough for a document to load on a slow connection, and short enough that a reader on no
 *  document at all is not left waiting for a recovery that cannot come. Exported for the margin
 *  row that drives that bound. */
export const BRIDGE_BIND_WAIT_MS = 10_000;

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
  /** What the composer says in place of the server's words for a send it refused for a mark it
   *  cannot see, kept with the mark it answers: why the margin could not write that mark again
   *  (`REANCHOR_REFUSALS`, `DOCUMENT_CLOSED_REFUSAL`), or that it is still writing it
   *  (`REANCHOR_IN_PROGRESS`), while the recovery waits for a document to bind. A newer anchor
   *  leaves it behind. */
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

/** Where a composer registers the draft it holds, apart from the margin's own value: the margin's
 *  changes with every hover, selection and placement report, while this never does. Every
 *  `MentionComposer` on the page registers - the conversation tab's and `AgentMessageComposer`'s
 *  among them, which read nothing else of the margin - so reading it from the margin's value
 *  would re-render each of them on margin state none of them shows. */
const ComposerRegistry = createContext<(composer: ComposerBridge) => () => void>(
  // A composer outside a margin - a test's bare one, a surface the provider does not wrap -
  // registers with nobody, and nothing asks it for its draft; its own debounced write still
  // keeps it.
  () => () => {}
);

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
  // The marks whose recovery is running, which is set before the wait for a document to bind:
  // the reader can press Retry while that wait is out, the server refuses the same mark again,
  // and a second recovery for it would write a second mark once the bridge binds - one of the
  // two left in the shared document with no record of its own (dispatch://INFRA-727).
  const recovering = useRef(new Set<string>());
  // Mirrors of the open compose and `documentBridge` for the compose callbacks, which stay stable
  // (the editor holds `composeForMark` for the document's lifetime).
  const openCompose = useRef<OpenCompose | undefined>(undefined);
  const bridgeRef = useRef<DocumentBridge | undefined>(undefined);
  // Who waits for the next document to bind its bridge: a refused send whose document is still
  // loading (`whenBridgeBinds`). A wait only ever drops itself, which Set iteration allows.
  const bridgeWaiters = useRef(new Set<(bridge: DocumentBridge) => void>());
  // The composers on screen that hold a draft, by the name of the send that draft belongs to.
  const composers = useRef(new Map<string, ComposerBridge>());
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
  // The bridge of the document on screen, waiting while no document has registered one at all:
  // the reader may be on the very document a refused send is anchored to, with its editor still
  // being built, and telling them it is not open would be false - the recovery is seconds away.
  // A page that is on no document registers nothing, so the wait ends at `BRIDGE_BIND_WAIT_MS`
  // with the bridge as it stands then - none - and the reader gets a refusal they can act on. A
  // bridge already registered answers at once, this document's or another's: which document is
  // on screen is then known, and no waiting makes the compose's own document the one here.
  const whenBridgeBinds = useCallback((): Promise<DocumentBridge | undefined> => {
    if (bridgeRef.current !== undefined) return Promise.resolve(bridgeRef.current);
    return new Promise<DocumentBridge | undefined>((resolve) => {
      const waiter = (bridge: DocumentBridge) => {
        bridgeWaiters.current.delete(waiter);
        clearTimeout(bound);
        resolve(bridge);
      };
      const bound = setTimeout(() => {
        bridgeWaiters.current.delete(waiter);
        resolve(bridgeRef.current);
      }, BRIDGE_BIND_WAIT_MS);
      bridgeWaiters.current.add(waiter);
    });
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
      const markId = held.request.anchor?.mark_id;
      if (markId === undefined || recovering.current.has(markId)) return;
      recovering.current.add(markId);
      try {
        // What the reader is told while the margin waits for a document: the server's sentence
        // is about a mark they never saw, and this one is about the draft in front of them.
        setAnchorRefusal({ markId, text: REANCHOR_IN_PROGRESS(held.request.kind) });
        const bridge = await whenBridgeBinds();
        // The held send as it stands now, which is what the retry will carry: while the wait
        // was out the reader could discard the refusal - the draft a mark would belong to is
        // gone - or type into it, and the store holds what they typed. A send that is no longer
        // refused under this mark is not this recovery's to re-address.
        const current = store.get(held.mutationKey);
        if (current?.status !== "refused") return;
        const request = current.request;
        const anchor = request.anchor;
        if (anchor?.mark_id !== markId) return;
        const kind = request.kind;
        // The compose's own document is not the one on screen, so there is no editor here to
        // write its mark again. Nothing about the words is known from here - saying they were
        // changed or deleted would send the reader looking for an edit nobody made - and opening
        // that document is what lets the margin attach this draft.
        if (bridge === undefined || bridge.artifactId !== anchor.artifact) {
          setAnchorRefusal({ markId, text: DOCUMENT_CLOSED_REFUSAL(kind) });
          return;
        }
        // What the refused send itself carries: the words the composer was opened over, and the
        // block and surrounding text the editor wrote its mark among. Both outlive the mark and
        // the editor state that wrote it, which is why the composer is the one that keeps them.
        const outcome = bridge.remarkMark(markId, kind, anchor.quote, anchor.placement);
        if ("refused" in outcome) {
          setAnchorRefusal({ markId, text: REANCHOR_REFUSALS[outcome.refused](kind) });
          return;
        }
        remarked.current.add(outcome.markId);
        setAnchorRefusal(undefined);
        store.readdress(held.mutationKey, { ...request, anchor: anchorAt(anchor, outcome) });
        await bridge.whenSent(REANCHOR_SEND_WAIT_MS);
        // The retry carries the draft the store holds, and the composer writes what the reader
        // has typed there only a short wait after each keystroke (`KEEP_DRAFT_MS`): words typed
        // while the margin waited would go nowhere, and the landed send would then clear the
        // composer they are in. So the composer writes its draft now, before the retry takes it.
        composers.current.get(hashKey(held.mutationKey))?.keepDraft();
        store.retry(held.mutationKey);
      } finally {
        // The next refusal of this mark is the reader's to recover from again: a recovery that
        // refused leaves the draft where it is, and one that sent has a mark of its own in
        // `remarked` to answer for it.
        recovering.current.delete(markId);
      }
    },
    [store, whenBridgeBinds]
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
      const retyped = anchorAt(anchor, outcome);
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
      // A document that has just bound its editor answers whoever waited for one; an
      // unregistration answers nobody - the next document, or the wait's own bound, does.
      if (bridge !== undefined) {
        for (const waiter of bridgeWaiters.current) waiter(bridge);
      }
    },
    [withdrawPlacements]
  );
  // A composer registers while it is mounted and takes itself back as it goes; the one that
  // mounts next under the same name is this map's then. A send whose composer registered
  // nothing - no composer is on screen for it - is sent again with the draft the store holds.
  const registerComposer = useCallback((composer: ComposerBridge) => {
    const name = hashKey(composer.mutationKey);
    composers.current.set(name, composer);
    return () => {
      if (composers.current.get(name) === composer) composers.current.delete(name);
    };
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

  return (
    <ComposerRegistry.Provider value={registerComposer}>
      <MarginContext.Provider value={value}>{children}</MarginContext.Provider>
    </ComposerRegistry.Provider>
  );
}

export function useMargin(): MarginContextValue {
  return useContext(MarginContext);
}

/** Takes the composer that holds the draft of a held send, for as long as it is mounted, and
 *  answers what unregisters it. The margin asks the composer of the send it is about to send
 *  again for what the reader has typed since the refusal (`reanchor`). */
export function useComposerRegistry(): (composer: ComposerBridge) => () => void {
  return useContext(ComposerRegistry);
}

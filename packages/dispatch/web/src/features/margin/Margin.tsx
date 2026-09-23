import { itemFromSearch } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  type ReactNode,
  type RefObject,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useLocation } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { whoAmIQuery } from "../../api/queries";
import type { Artifact, Ask, Event, UserState } from "../../api/types";
import {
  borderDefault,
  focusVisibleRing,
  railBg,
  railBorder,
  railHoverBg,
  railText,
} from "../../theme/classes";
import { isRetractedAsk } from "../conversation/conversation-model";
import type { ComposerAnchor } from "../conversation/MentionComposer";
import { pulseBlock } from "../doc/marks";
import { useProjectArtifact } from "../document/useProjectArtifact";
import { stateForIssue } from "../issue/IssueHeader";
import { eventItemId } from "../issue/pins";
import { applyPinStateOperation, sharedIssueStateWrites } from "../issue/state-write-queue";
import { parseIssuePath, parseProjectPath } from "../refs/routes";
import { COMPACT_VIEWPORT_QUERY, PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import type { MarginComposer } from "./CommentsTab";
import { MarginSheet } from "./MarginSheet";
import {
  type MarginItem,
  type MarginItemAction,
  type MarginOwner,
  type MarginTab,
  type MarkPlacement,
  marginItemId,
  marginItemMarkId,
  type Thread,
  threadMarkId,
  useMarginItems,
  useMarginOwner,
} from "./useMarginItems";
import { useMarginListeners } from "./useMarginListeners";

export const DEFAULT_MARGIN_WIDTH = 384;
const MIN_MARGIN_WIDTH = 280;
const MARGIN_WIDTH_STEP = 24;

/** The widest the margin may be dragged: 60 % of the viewport, never below the minimum. */
function maxMarginWidth(): number {
  return Math.max(MIN_MARGIN_WIDTH, Math.floor(window.innerWidth * 0.6));
}

function clampMarginWidth(width: number): number {
  return Math.min(Math.max(width, MIN_MARGIN_WIDTH), maxMarginWidth());
}

/** The margin item a focus request names, once the request's mark has an item. */
function focusedItemFor(
  items: readonly MarginItem[],
  request: { markId: string; seq: number } | undefined
): { itemId: string; seq: number } | undefined {
  if (request === undefined) {
    return undefined;
  }
  const item = items.find((candidate) => marginItemMarkId(candidate) === request.markId);
  return item === undefined ? undefined : { itemId: marginItemId(item), seq: request.seq };
}

export interface DocumentBridge {
  focusBlock(blockId: string): void;
  focusMark(markId: string): void;
  setActiveBlocks(blockIds: readonly string[]): void;
  setActiveMarks(markIds: readonly string[]): void;
}

export interface MarkComposeRequest {
  anchor: ComposerAnchor;
  kind: "ask" | "comment" | "suggestion";
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
  replaceCompose(): void;
  selectItem(id: string): void;
  selectedItemId: string | undefined;
  setBlockPlacements(placements: ReadonlyMap<string, MarkPlacement>): void;
  setHoveredItemId(id: string | undefined): void;
  setMarkItemIds(markItemIds: ReadonlyMap<string, string>): void;
  setMarkPlacements(placements: ReadonlyMap<string, MarkPlacement>): void;
  settleCompose(outcome: "saved" | "cancelled"): void;
}

export interface MarginSheetModel {
  actions: {
    closeComposer: () => void;
    onAction: (id: string, action: MarginItemAction) => void;
    onComposerSaved: () => void;
    onEdit: (id: string, body: string) => Promise<unknown>;
    onRetryAction: () => void;
    onUnpin: (eventId: number) => void;
    onRetryAnsweredAsk: (() => void) | undefined;
    onRetryComments: () => void;
    onRetryIssue: () => void;
    onToggleThread: (key: string) => void;
    onEditingChange: (id: string | undefined) => void;
    onToggleResolved: () => void;
  };
  composer: MarginComposer | undefined;
  items: {
    actionErrorId: string | undefined;
    answeredAsksPending: boolean;
    asksPending: boolean;
    commentsError: boolean;
    commentsPending: boolean;
    historicalAsks: Ask[];
    marginRef: RefObject<HTMLElement | null>;
    isClosed: boolean;
    issueError: boolean;
    owner: MarginOwner | undefined;
    issuePending: boolean;
    needsYou: Ask[];
    onSelectCard: (id: string, blockID?: string) => void;
    openAskCount: number;
    pendingActionIds: ReadonlySet<string>;
    pinned: Event[];
    pinnedIds: string[];
    resolvedThreads: Thread[];
    /** Retracted asks hidden behind the same toggle as resolved threads. */
    retractedAskCount: number;
    threads: Thread[];
    viewerLogin: string;
    visibleArtifact: Artifact | undefined;
  };
  placement: {
    blockPlacements: ReadonlyMap<string, MarkPlacement>;
    markPlacements: ReadonlyMap<string, MarkPlacement>;
  };
  selection: {
    expandedThreadKey: string | undefined;
    editingCommentId: string | undefined;
    savingCommentEditId: string | undefined;
    hoveredItemId: string | undefined;
    hoveredMarkId: string | undefined;
    selectedItemId: string | undefined;
    showResolved: boolean;
  };
  sheet: {
    closeThread: () => void;
    expanded: boolean;
    threadKey: string | undefined;
    toggle: (expanded?: boolean) => void;
  };
  filter: {
    blockId: string | undefined;
    clear(): void;
  };
  tab: {
    set: (tab: MarginTab) => void;
    value: MarginTab;
  };
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
  replaceCompose: unavailableMargin,
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

  const composeForMark = useCallback(
    (request: MarkComposeRequest): Promise<void> =>
      new Promise<void>((resolve, reject) => {
        composePromise.current?.reject(new Error("replaced by a newer composer"));
        composePromise.current = { reject, resolve };
        sequence.current += 1;
        setPendingCompose({ ...request, seq: sequence.current });
      }),
    []
  );
  const settleCompose = useCallback((outcome: "saved" | "cancelled") => {
    const pending = composePromise.current;
    composePromise.current = undefined;
    setPendingCompose(undefined);
    if (pending === undefined) {
      return;
    }
    if (outcome === "saved") {
      pending.resolve();
      return;
    }
    pending.reject(new Error("composer closed"));
  }, []);
  const replaceCompose = useCallback(() => {
    const pending = composePromise.current;
    composePromise.current = undefined;
    setPendingCompose(undefined);
    pending?.reject(new Error("replaced by a newer composer"));
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
      replaceCompose,
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
      replaceCompose,
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

function useMarginSheet(): MarginSheetModel {
  const {
    blockFilterId,
    blockPlacements,
    clearBlockFilter,
    documentBridge,
    focusBlock,
    focusRequest,
    hoveredItemId,
    hoveredMarkId,
    markPlacements,
    pendingCompose,
    placementsReported,
    selectItem,
    selectedItemId,
    setHoveredItemId,
    setMarkItemIds,
    settleCompose,
  } = useMargin();
  const queryClient = useQueryClient();
  const { key: locationKey, pathname, search } = useLocation();
  const issueRoute = parseIssuePath(pathname, search);
  const projectRoute = parseProjectPath(pathname, search);
  const documentRoute = projectRoute?.kind === "document" ? projectRoute : undefined;
  const owner = useMarginOwner();
  const issueKey = owner?.kind === "issue" ? owner.key : undefined;
  const documentArtifact = useProjectArtifact(documentRoute);
  const routeArtifactSlug = issueRoute?.kind === "artifact" ? issueRoute.slug : documentRoute?.slug;
  const queryItem = itemFromSearch(search);
  const routeItemId =
    issueRoute?.kind === "ask" || issueRoute?.kind === "comment"
      ? issueRoute.id
      : (documentRoute?.item?.id ?? queryItem?.id);
  // Each navigation is its own request for the item it names, even when it names the one the
  // reader is already on: following a link back to the card you moved off has to bring you back.
  // `useLocation().key` changes on a same-URL push, which an item id alone cannot see.
  const routeItemKey = routeItemId === undefined ? undefined : `${locationKey}:${routeItemId}`;

  // The URL names the selection until the reader makes one: the link's item is rendered selected
  // from the first commit, before the asynchronous selection effect has caught the shared margin
  // state up. It never outranks the reader, though - nothing strips `?comment=` from the URL, so
  // reading it first would pin the linked card as selected for the rest of the visit.
  const displayedSelectedItemId = selectedItemId ?? routeItemId;
  const [tab, setTab] = useState<MarginTab>("comments");
  const [composer, setComposer] = useState<MarginComposer>();
  const [expandedOwnerId, setExpandedOwnerId] = useState<string>();
  const [expandedThreadKey, setExpandedThreadKey] = useState<string>();
  const [editingCommentId, setEditingCommentId] = useState<string>();
  const [savingCommentEditId, setSavingCommentEditId] = useState<string>();
  const [sheetThreadKey, setSheetThreadKey] = useState<string>();
  const [showResolved, setShowResolved] = useState(false);
  const marginRef = useRef<HTMLElement>(null);
  const issue = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["issue", issueKey],
    queryFn: () => {
      if (issueKey === undefined) {
        throw new Error("Issue margin query requires an issue owner.");
      }
      return api.getIssue(issueKey);
    },
  });
  const viewer = useQuery(whoAmIQuery());
  const visibleArtifact =
    documentRoute === undefined
      ? routeArtifactSlug === undefined
        ? primarySpec(issue.data)
        : issue.data?.artifacts.find((artifact) => artifact.slug === routeArtifactSlug)
      : documentArtifact.data;
  const unpin = useCallback(
    (eventId: number) => {
      if (owner?.kind !== "issue") {
        return;
      }
      const operation = { id: eventItemId({ id: eventId }), op: "unpin" as const };
      queryClient.setQueryData<UserState>(["user-state"], (current) => {
        const issueState = stateForIssue(current, owner.key);
        return {
          ...current,
          [owner.key]: {
            ...issueState,
            dismissed: applyPinStateOperation(issueState.dismissed, operation),
          },
        };
      });
      void sharedIssueStateWrites
        .enqueue(owner.key, operation, {
          fetchState: async (key) => stateForIssue(await api.getMyState(), key),
          onDrained: (key, next) => {
            queryClient.setQueryData<UserState>(["user-state"], (current) => ({
              ...current,
              [key]: next,
            }));
          },
          onError: () => {
            void queryClient.invalidateQueries({ queryKey: ["user-state"] });
          },
          putState: (key, state) => api.putIssueState(key, state),
          staleState: (error) =>
            error instanceof ApiError && error.code === "STATE_STALE" ? error.state : undefined,
        })
        .catch(() => {});
    },
    [owner, queryClient]
  );
  const {
    actionErrorId,
    answeredAsksPending,
    asksPending,
    commentsError,
    commentsPending,
    editComment,
    items,
    marginItems,
    mutateItem,
    needsYou,
    pendingActionIds,
    openAskCount,
    pinned,
    pinnedIds,
    resolvedThreads,
    retryAnsweredAsk,
    retryComments,
    retryItem,
    threads,
  } = useMarginItems(owner, tab, visibleArtifact, markPlacements, blockPlacements, blockFilterId);
  const onEdit = useCallback(
    async (id: string, body: string) => {
      setSavingCommentEditId(id);
      try {
        return await editComment(id, body);
      } finally {
        setSavingCommentEditId(undefined);
      }
    },
    [editComment]
  );

  // A retracted ask is withdrawn history; it rides the same "show resolved" toggle as
  // resolved comment threads instead of sitting among the answered decisions.
  const askItems = useMemo(
    () =>
      items
        .filter((item): item is Extract<MarginItem, { kind: "ask" }> => item.kind === "ask")
        .map((item) => item.ask),
    [items]
  );
  const retractedAskCount = useMemo(
    () => askItems.filter((ask) => isRetractedAsk(ask)).length,
    [askItems]
  );
  const historicalAsks = useMemo(
    () => askItems.filter((ask) => showResolved || !isRetractedAsk(ask)),
    [askItems, showResolved]
  );
  const markItemIds = useMemo(() => {
    const ids = new Map<string, string>();
    for (const item of marginItems) {
      const markId = marginItemMarkId(item);
      if (markId !== undefined) {
        ids.set(markId, marginItemId(item));
      }
    }
    return ids;
  }, [marginItems]);
  useEffect(() => {
    setMarkItemIds(markItemIds);
    return () => setMarkItemIds(new Map());
  }, [markItemIds, setMarkItemIds]);
  const isClosed =
    owner?.kind === "document" ? false : issue.data !== undefined && issue.data.closed_at !== null;
  const ownerId =
    owner?.kind === "issue" ? owner.key : owner?.kind === "document" ? owner.artifactId : undefined;
  // Every owner opens on Comments; a Pinned selection does not follow the reader to the next issue.
  useEffect(() => {
    if (ownerId !== undefined) {
      setTab("comments");
    }
  }, [ownerId]);
  // `openPhoneThread`: a mark click or card tap opens the thread in the phone's dialog; a
  // route-driven selection (a comment or ask deep link) only highlights the card — on a phone
  // the link's destination is the Conversation turn, which a modal dialog would cover.
  const selectMarginItem = useCallback(
    (id: string, openPhoneThread = true) => {
      selectItem(id);
      const thread = [...threads, ...resolvedThreads].find((candidate) => candidate.key === id);
      if (thread === undefined) {
        setExpandedThreadKey(undefined);
        return;
      }
      if (thread.resolved) {
        setShowResolved(true);
      }
      if (ownerId !== undefined && window.matchMedia(PHONE_VIEWPORT_QUERY).matches) {
        // A phone thread lives in the Thread dialog, never expanded inline (`onToggleThread`
        // keeps the same rule). A document item URL opens the review panel so the highlighted
        // card is reachable, but leaves the thread collapsed.
        if (openPhoneThread) {
          setExpandedThreadKey(thread.key);
          setExpandedOwnerId(ownerId);
          setSheetThreadKey(thread.key);
        } else if (owner?.kind === "document") {
          setExpandedOwnerId(ownerId);
        }
        return;
      }
      setExpandedThreadKey(thread.key);
    },
    [owner, ownerId, resolvedThreads, selectItem, threads]
  );
  const onToggleThread = useCallback(
    (key: string) => {
      const thread = [...threads, ...resolvedThreads].find((candidate) => candidate.key === key);
      if (thread === undefined) {
        return;
      }
      selectItem(key);
      if (thread.resolved) {
        setShowResolved(true);
      }
      const blockID = thread.anchor?.block_id;
      if (typeof blockID === "string") {
        selectedBlockFocus.current = undefined;
        focusBlock(blockID);
      }
      if (!thread.anchor?.orphaned) {
        const markId = threadMarkId(thread);
        if (markId !== undefined) {
          documentBridge?.focusMark(markId);
        }
      }
      if (ownerId !== undefined && window.matchMedia(PHONE_VIEWPORT_QUERY).matches) {
        setExpandedOwnerId(ownerId);
        setSheetThreadKey(key);
        return;
      }
      setExpandedThreadKey((current) => (current === key ? undefined : key));
    },
    [documentBridge, focusBlock, ownerId, resolvedThreads, selectItem, threads]
  );
  const sheetExpanded = ownerId !== undefined && expandedOwnerId === ownerId;
  const toggleSheet = useCallback(
    (expanded?: boolean) => {
      const nextExpanded = expanded ?? !sheetExpanded;
      setExpandedOwnerId(nextExpanded ? ownerId : undefined);
      if (!nextExpanded) {
        setSheetThreadKey(undefined);
      }
    },
    [ownerId, sheetExpanded]
  );
  const closeComposer = useCallback(() => {
    setComposer(undefined);
    settleCompose("cancelled");
  }, [settleCompose]);
  const onComposerSaved = useCallback(() => {
    settleCompose("saved");
  }, [settleCompose]);

  useEffect(() => {
    if (pendingCompose === undefined) {
      return;
    }
    setTab("comments");
    setComposer({ anchor: pendingCompose.anchor, kind: pendingCompose.kind });
    if (ownerId !== undefined && window.matchMedia(COMPACT_VIEWPORT_QUERY).matches) {
      setExpandedOwnerId(ownerId);
    }
  }, [ownerId, pendingCompose]);
  useEffect(() => {
    if (
      composer !== undefined &&
      (isClosed ||
        (composer.anchor !== undefined && composer.anchor.artifact !== visibleArtifact?.id))
    ) {
      closeComposer();
    }
  }, [closeComposer, composer, isClosed, visibleArtifact?.id]);
  // A project-document item link has nowhere else to land, so the compact sheet opens on it.
  // An issue's comment or ask deep link lands on its Conversation turn; the margin selects the
  // card without covering that turn with the sheet.
  const documentItemId = documentRoute?.item?.id;
  useEffect(() => {
    if (
      documentItemId !== undefined &&
      ownerId !== undefined &&
      window.matchMedia(COMPACT_VIEWPORT_QUERY).matches
    ) {
      setExpandedOwnerId(ownerId);
    }
  }, [ownerId, documentItemId]);
  useEffect(() => {
    if (blockFilterId === undefined) {
      return;
    }
    setTab("comments");
    if (ownerId !== undefined && window.matchMedia(COMPACT_VIEWPORT_QUERY).matches) {
      setExpandedOwnerId(ownerId);
    }
  }, [blockFilterId, ownerId]);

  const handledFocusSequence = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (focusRequest === undefined) {
      handledFocusSequence.current = undefined;
      return;
    }
    if (handledFocusSequence.current === focusRequest.seq) {
      return;
    }
    const item = marginItems.find(
      (candidate) => marginItemMarkId(candidate) === focusRequest.markId
    );
    if (item === undefined) {
      return;
    }
    handledFocusSequence.current = focusRequest.seq;
    selectMarginItem(marginItemId(item));
    setTab("comments");
    if (ownerId !== undefined && window.matchMedia(COMPACT_VIEWPORT_QUERY).matches) {
      setExpandedOwnerId(ownerId);
    }
  }, [focusRequest, marginItems, ownerId, selectMarginItem]);
  useEffect(() => {
    const selectedItems = [selectedItemId, hoveredItemId]
      .map((itemId) => marginItems.find((candidate) => marginItemId(candidate) === itemId))
      .filter((item): item is MarginItem => item !== undefined);
    const markIds = selectedItems
      .map((item) => marginItemMarkId(item))
      .filter((markId): markId is string => markId !== undefined);
    const blockIds = selectedItems
      .map((item) => (item.kind === "ask" ? item.ask.anchor : item.comment.anchor))
      .flatMap((anchor) =>
        anchor?.orphaned && typeof anchor.block_id === "string" ? [anchor.block_id] : []
      );
    documentBridge?.setActiveMarks([...new Set(markIds)]);
    documentBridge?.setActiveBlocks(blockIds);
  }, [documentBridge, hoveredItemId, marginItems, selectedItemId]);

  const onAction = useCallback(
    (id: string, action: MarginItemAction) => {
      mutateItem({ id, kind: action });
    },
    [mutateItem]
  );
  const selectedBlockFocus = useRef<string | undefined>(undefined);
  useEffect(() => {
    const item = marginItems.find((candidate) => marginItemId(candidate) === selectedItemId);
    const anchor =
      item === undefined ? undefined : item.kind === "ask" ? item.ask.anchor : item.comment.anchor;
    if (!anchor?.orphaned || typeof anchor.block_id !== "string") {
      selectedBlockFocus.current = undefined;
      return;
    }
    if (selectedBlockFocus.current === anchor.block_id) {
      return;
    }
    selectedBlockFocus.current = anchor.block_id;
    focusBlock(anchor.block_id);
  }, [focusBlock, marginItems, selectedItemId]);
  const onSelectCard = useCallback(
    (id: string) => {
      selectMarginItem(id);
      const item = marginItems.find((candidate) => marginItemId(candidate) === id);
      const markId = item === undefined ? undefined : marginItemMarkId(item);
      if (markId !== undefined) {
        documentBridge?.focusMark(markId);
      }
    },
    [documentBridge, marginItems, selectMarginItem]
  );
  const focus = focusedItemFor(marginItems, focusRequest);

  // A document item URL is often the first page the reader loads. Its thread arrives after the
  // route effect's first pass, so apply that selection once the margin has the item, and bring
  // the quote the link names into the document's viewport the way a fragment link would - on a
  // phone, where the margin is a sheet over the document, and on a desktop, where the quote can
  // be thousands of pixels below the fold. Once, per link: the URL keeps naming its item for as
  // long as the reader stays on the page, and re-applying it would take the selection back off
  // whatever card they went on to click.
  const appliedRouteItem = useRef<string | undefined>(undefined);
  const focusedRouteMark = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (routeItemId === undefined || routeItemKey === undefined) {
      appliedRouteItem.current = undefined;
      focusedRouteMark.current = undefined;
      return;
    }
    const item = marginItems.find((candidate) => marginItemId(candidate) === routeItemId);
    if (item === undefined) {
      return;
    }
    // The selection is applied once per link; the quote is still brought into view the first
    // time the open document can be asked, which is often a later pass than this one.
    if (appliedRouteItem.current !== routeItemKey) {
      appliedRouteItem.current = routeItemKey;
      selectMarginItem(routeItemId, false);
    }
    const markId = marginItemMarkId(item);
    // Only once the open document has reported that mark. `focusMark` is a one-shot scroll into
    // a span that has to exist: asked while the document is still projecting its marks - which
    // is where a client-side navigation lands - it silently does nothing and the reader never
    // sees the quote. A published placement is the document saying the span is rendered.
    if (
      markId === undefined ||
      !markPlacements.has(markId) ||
      focusedRouteMark.current === `${routeItemKey}:${markId}` ||
      documentBridge === undefined
    ) {
      return;
    }
    focusedRouteMark.current = `${routeItemKey}:${markId}`;
    documentBridge.focusMark(markId);
  }, [documentBridge, marginItems, markPlacements, routeItemId, routeItemKey, selectMarginItem]);

  useMarginListeners({
    composerOpen: composer !== undefined,
    focus,
    margin: marginRef,
    onSelectCard,
    placementsPublished: placementsReported,
    routeItemId,
    routeItemKey,
    setHoveredItemId,
    setTab,
    sheetExpanded,
    tab,
    visibleArtifact,
  });

  return {
    actions: {
      onEditingChange: setEditingCommentId,
      closeComposer,
      onAction,
      onComposerSaved,
      onEdit,
      onUnpin: unpin,
      onRetryAction: retryItem,
      onRetryAnsweredAsk: retryAnsweredAsk,
      onRetryComments: retryComments,
      onRetryIssue: () => void issue.refetch(),
      onToggleResolved: () => setShowResolved((current) => !current),
      onToggleThread,
    },
    composer,
    items: {
      actionErrorId,
      answeredAsksPending,
      asksPending,
      commentsError,
      commentsPending,
      historicalAsks,
      marginRef,
      isClosed,
      issueError: issue.isError,
      owner,
      issuePending: issue.isPending,
      needsYou,
      onSelectCard,
      openAskCount,
      pendingActionIds,
      pinned,
      pinnedIds,
      resolvedThreads,
      retractedAskCount,
      threads,
      viewerLogin: viewer.data?.login ?? "",
      visibleArtifact,
    },
    filter: {
      blockId: blockFilterId,
      clear: clearBlockFilter,
    },
    placement: {
      blockPlacements,
      markPlacements,
    },
    selection: {
      expandedThreadKey,
      editingCommentId,
      savingCommentEditId,
      hoveredItemId,
      hoveredMarkId,
      selectedItemId: displayedSelectedItemId,
      showResolved,
    },
    sheet: {
      closeThread: () => setSheetThreadKey(undefined),
      expanded: sheetExpanded,
      threadKey: sheetThreadKey,
      toggle: toggleSheet,
    },
    tab: {
      set: setTab,
      value: tab,
    },
  };
}

interface MarginProps {
  collapsed?: boolean;
  onCollapsedChange?(collapsed: boolean): void;
  onWidthChange?(width: number): void;
  width?: number;
}

export function Margin({
  collapsed = false,
  onCollapsedChange,
  onWidthChange,
  width,
}: MarginProps): ReactNode {
  const model = useMarginSheet();
  const isCompactViewport = useMediaQuery(COMPACT_VIEWPORT_QUERY);
  const resizePointer = useRef<
    { pointerId: number; startWidth: number; startX: number } | undefined
  >(undefined);
  const marginWidth = clampMarginWidth(width ?? DEFAULT_MARGIN_WIDTH);
  const maxWidth = maxMarginWidth();
  const setClampedMarginWidth = (nextWidth: number) => {
    onWidthChange?.(clampMarginWidth(nextWidth));
  };

  if (!isCompactViewport && collapsed) {
    return (
      <aside
        aria-label="Collapsed margin"
        className={`fixed inset-y-0 right-0 z-10 flex w-14 justify-center border-l p-2 ${railBorder} ${railBg} ${railText}`}
        data-testid="margin-rail"
      >
        <button
          aria-label="Show margin"
          className={`min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
          onClick={() => onCollapsedChange?.(false)}
          type="button"
        >
          <span aria-hidden="true">‹</span>
        </button>
      </aside>
    );
  }

  return (
    <div
      className={isCompactViewport ? "contents" : "relative order-3 shrink-0"}
      data-testid={isCompactViewport ? undefined : "desktop-margin-shell"}
      style={isCompactViewport ? undefined : { width: `${marginWidth}px` }}
    >
      <hr
        aria-controls="review-margin"
        aria-label="Resize margin"
        aria-orientation="vertical"
        aria-valuemax={maxWidth}
        aria-valuemin={MIN_MARGIN_WIDTH}
        aria-valuenow={marginWidth}
        className={
          isCompactViewport
            ? "hidden"
            : `absolute top-0 -left-1 z-20 h-full w-2 border-0 border-l cursor-col-resize focus-visible:outline-none focus-visible:ring-2 ${borderDefault} ${focusVisibleRing}`
        }
        onDoubleClick={() => setClampedMarginWidth(DEFAULT_MARGIN_WIDTH)}
        onKeyDown={(event) => {
          if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") {
            return;
          }
          event.preventDefault();
          setClampedMarginWidth(
            marginWidth + (event.key === "ArrowLeft" ? MARGIN_WIDTH_STEP : -MARGIN_WIDTH_STEP)
          );
        }}
        onPointerCancel={(event) => {
          if (resizePointer.current?.pointerId === event.pointerId) {
            resizePointer.current = undefined;
          }
        }}
        onPointerDown={(event) => {
          if (event.button !== 0) {
            return;
          }
          event.preventDefault();
          event.currentTarget.setPointerCapture(event.pointerId);
          resizePointer.current = {
            pointerId: event.pointerId,
            startWidth: marginWidth,
            startX: event.clientX,
          };
        }}
        onPointerMove={(event) => {
          const pointer = resizePointer.current;
          if (pointer?.pointerId !== event.pointerId) {
            return;
          }
          setClampedMarginWidth(pointer.startWidth + pointer.startX - event.clientX);
        }}
        onPointerUp={(event) => {
          if (resizePointer.current?.pointerId !== event.pointerId) {
            return;
          }
          resizePointer.current = undefined;
          event.currentTarget.releasePointerCapture(event.pointerId);
        }}
        tabIndex={0}
      />
      <MarginSheet
        desktopControl={
          isCompactViewport || onCollapsedChange === undefined ? undefined : (
            <button
              aria-label="Hide margin"
              className={`min-h-11 min-w-11 rounded-lg text-sm font-medium ${railHoverBg}`}
              onClick={() => onCollapsedChange(true)}
              type="button"
            >
              <span aria-hidden="true">›</span>
            </button>
          )
        }
        model={model}
      />
    </div>
  );
}

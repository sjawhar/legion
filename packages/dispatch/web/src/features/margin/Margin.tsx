import { useQuery } from "@tanstack/react-query";
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

import { api } from "../../api/client";
import type { Artifact, Ask, Event } from "../../api/types";
import { parseIssuePath, parseProjectPath } from "../refs/routes";
import type { MarginComposer } from "./CommentsTab";
import type { ComposerAnchor } from "./Composer";
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
    pendingActionId: string | undefined;
    pinned: Event[];
    pinnedIds: string[];
    resolvedThreads: Thread[];
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

function pulseBlock(blockId: string): void {
  const escaped =
    typeof CSS !== "undefined" && typeof CSS.escape === "function"
      ? CSS.escape(blockId)
      : blockId.replace(/["\\]/g, "\\$&");
  const block = document.querySelector<HTMLElement>(`[data-block-id="${escaped}"]`);
  if (block === null) {
    return;
  }
  block.scrollIntoView({ behavior: "smooth", block: "center" });
  block.classList.add("dispatch-mark-pulse");
  window.setTimeout(() => block.classList.remove("dispatch-mark-pulse"), 1200);
}
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
  const registerDocument = useCallback((bridge: DocumentBridge | undefined) => {
    setDocumentBridge(bridge);
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
      registerDocument,
      replaceCompose,
      selectItem: setSelectedItemId,
      selectedItemId,
      setBlockPlacements,
      setHoveredItemId: selectHoveredItem,
      setMarkItemIds,
      setMarkPlacements,
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
    selectItem,
    selectedItemId,
    setHoveredItemId,
    setMarkItemIds,
    settleCompose,
  } = useMargin();
  const { pathname, search } = useLocation();
  const issueRoute = parseIssuePath(pathname, search);
  const projectRoute = parseProjectPath(pathname, search);
  const documentRoute = projectRoute?.kind === "document" ? projectRoute : undefined;
  const owner = useMarginOwner();
  const issueKey = owner?.kind === "issue" ? owner.key : undefined;
  const documentArtifact = useQuery({
    enabled: documentRoute !== undefined,
    queryKey: ["artifact-ref", `${documentRoute?.project}/${documentRoute?.slug}`],
    queryFn: () => {
      if (documentRoute === undefined) {
        throw new Error("Project document query requires a document route.");
      }
      return api.getProjectArtifact(documentRoute.project, documentRoute.slug);
    },
  });
  const routeArtifactSlug = issueRoute?.kind === "artifact" ? issueRoute.slug : documentRoute?.slug;
  const routeItemId =
    issueRoute?.kind === "ask" || issueRoute?.kind === "comment"
      ? issueRoute.id
      : documentRoute?.item?.id;
  const [tab, setTab] = useState<MarginTab>("comments");
  const [composer, setComposer] = useState<MarginComposer>();
  const [expandedOwnerId, setExpandedOwnerId] = useState<string>();
  const [expandedThreadKey, setExpandedThreadKey] = useState<string>();
  const [editingCommentId, setEditingCommentId] = useState<string>();
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
  const viewer = useQuery({
    queryKey: ["whoami"],
    queryFn: () => api.whoAmI(),
  });
  const visibleArtifact =
    documentRoute === undefined
      ? routeArtifactSlug === undefined
        ? issue.data?.artifacts.find((artifact) => artifact.id === issue.data?.primary_artifact_id)
        : issue.data?.artifacts.find((artifact) => artifact.slug === routeArtifactSlug)
      : documentArtifact.data;
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
    pendingActionId,
    openAskCount,
    pinned,
    pinnedIds,
    resolvedThreads,
    retryAnsweredAsk,
    retryComments,
    retryItem,
    threads,
  } = useMarginItems(owner, tab, visibleArtifact, markPlacements, blockPlacements, blockFilterId);

  const historicalAsks = useMemo(
    () =>
      items
        .filter((item): item is Extract<MarginItem, { kind: "ask" }> => item.kind === "ask")
        .map((item) => item.ask),
    [items]
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
  const selectMarginItem = useCallback(
    (id: string) => {
      selectItem(id);
      const thread = [...threads, ...resolvedThreads].find((candidate) => candidate.key === id);
      if (thread === undefined) {
        setExpandedThreadKey(undefined);
        return;
      }
      if (thread.resolved) {
        setShowResolved(true);
      }
      setExpandedThreadKey(thread.key);
      if (ownerId !== undefined && window.matchMedia("(max-width: 767px)").matches) {
        setExpandedOwnerId(ownerId);
        setSheetThreadKey(thread.key);
      }
    },
    [ownerId, resolvedThreads, selectItem, threads]
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
      if (ownerId !== undefined && window.matchMedia("(max-width: 767px)").matches) {
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
    if (ownerId !== undefined && window.matchMedia("(max-width: 1279px)").matches) {
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
  useEffect(() => {
    if (
      routeItemId !== undefined &&
      ownerId !== undefined &&
      window.matchMedia("(max-width: 1279px)").matches
    ) {
      setExpandedOwnerId(ownerId);
    }
  }, [ownerId, routeItemId]);
  useEffect(() => {
    if (blockFilterId === undefined) {
      return;
    }
    setTab("comments");
    if (ownerId !== undefined && window.matchMedia("(max-width: 1279px)").matches) {
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
    if (ownerId !== undefined && window.matchMedia("(max-width: 1279px)").matches) {
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
  const focus =
    focusRequest === undefined
      ? undefined
      : (() => {
          const item = marginItems.find(
            (candidate) => marginItemMarkId(candidate) === focusRequest.markId
          );
          return item === undefined
            ? undefined
            : { itemId: marginItemId(item), seq: focusRequest.seq };
        })();

  useMarginListeners({
    focus,
    items: marginItems,
    margin: marginRef,
    onSelectCard,
    routeItemId,
    selectItem: selectMarginItem,
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
      onEdit: editComment,
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
      pendingActionId,
      pinned,
      pinnedIds,
      resolvedThreads,
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
      hoveredItemId,
      hoveredMarkId,
      selectedItemId,
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

export function Margin(): ReactNode {
  const model = useMarginSheet();

  return <MarginSheet model={model} />;
}

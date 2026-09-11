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
  focusMark(markId: string): void;
  setActiveMarks(markIds: readonly string[]): void;
}

export interface MarkComposeRequest {
  anchor: ComposerAnchor;
  kind: "ask" | "comment" | "suggestion";
}

interface MarginContextValue {
  composeForMark(request: MarkComposeRequest): Promise<void>;
  documentBridge: DocumentBridge | undefined;
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
    onSelectCard: (id: string) => void;
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
    markPlacements: ReadonlyMap<string, MarkPlacement>;
  };
  selection: {
    expandedThreadKey: string | undefined;
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
  tab: {
    set: (tab: MarginTab) => void;
    value: MarginTab;
  };
}

const unavailableMargin = (): never => {
  throw new Error("MarginProvider is required");
};
const MarginContext = createContext<MarginContextValue>({
  composeForMark: unavailableMargin,
  documentBridge: undefined,
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
  setHoveredItemId: unavailableMargin,
  setMarkItemIds: unavailableMargin,
  setMarkPlacements: unavailableMargin,
  settleCompose: unavailableMargin,
});

export function MarginProvider({ children }: { children: ReactNode }): ReactNode {
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
  const value = useMemo<MarginContextValue>(
    () => ({
      composeForMark,
      documentBridge,
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
      setHoveredItemId: selectHoveredItem,
      setMarkItemIds,
      setMarkPlacements,
      settleCompose,
    }),
    [
      composeForMark,
      documentBridge,
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
    documentBridge,
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
  } = useMarginItems(owner, tab, visibleArtifact, markPlacements);

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
      const markId = threadMarkId(thread);
      if (markId !== undefined) {
        documentBridge?.focusMark(markId);
      }
      if (ownerId !== undefined && window.matchMedia("(max-width: 767px)").matches) {
        setExpandedOwnerId(ownerId);
        setSheetThreadKey(key);
        return;
      }
      setExpandedThreadKey((current) => (current === key ? undefined : key));
    },
    [documentBridge, ownerId, resolvedThreads, selectItem, threads]
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
    const markIds = [selectedItemId, hoveredItemId]
      .map((itemId) => {
        const item = marginItems.find((candidate) => marginItemId(candidate) === itemId);
        return item === undefined ? undefined : marginItemMarkId(item);
      })
      .filter((markId): markId is string => markId !== undefined);
    documentBridge?.setActiveMarks([...new Set(markIds)]);
  }, [documentBridge, hoveredItemId, marginItems, selectedItemId]);

  const onAction = useCallback(
    (id: string, action: MarginItemAction) => {
      mutateItem({ id, kind: action });
    },
    [mutateItem]
  );
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
    placement: {
      markPlacements,
    },
    selection: {
      expandedThreadKey,
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

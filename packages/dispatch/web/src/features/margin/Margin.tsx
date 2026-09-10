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
import type { Artifact, Ask, Comment, Event } from "../../api/types";
import { parseIssuePath } from "../refs/routes";
import type { MarginComposer } from "./CommentsTab";
import type { ComposerAnchor, ComposerKind } from "./Composer";
import { MarginSheet } from "./MarginSheet";
import {
  type MarginItem,
  type MarginItemAction,
  type MarginTab,
  marginItemId,
  marginItemMarkId,
  threadRootId,
  useMarginItems,
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
  hoveredItemId: string | undefined;
  markPositions: ReadonlyMap<string, number>;
  pendingCompose: (MarkComposeRequest & { seq: number }) | undefined;
  registerDocument(bridge: DocumentBridge | undefined): void;
  replaceCompose(): void;
  selectItem(id: string): void;
  selectedItemId: string | undefined;
  setHoveredItemId(id: string | undefined): void;
  setMarkPositions(positions: ReadonlyMap<string, number>): void;
  settleCompose(outcome: "saved" | "cancelled"): void;
}

export interface MarginSheetModel {
  actions: {
    closeComposer: () => void;
    onAction: (id: string, action: MarginItemAction) => void;
    onComposerSaved: () => void;
    onReply: (comment: Comment) => void;
    onRetryAction: () => void;
    onRetryAnsweredAsk: (() => void) | undefined;
    onRetryComments: () => void;
    onRetryIssue: () => void;
  };
  composer: MarginComposer | undefined;
  items: {
    actionErrorId: string | undefined;
    answeredAsksPending: boolean;
    asksPending: boolean;
    comments: MarginItem[];
    commentsError: boolean;
    commentsPending: boolean;
    marginRef: RefObject<HTMLElement | null>;
    isClosed: boolean;
    issueError: boolean;
    issueKey: string | undefined;
    issuePending: boolean;
    needsYou: Ask[];
    onSelectCard: (id: string) => void;
    openAskCount: number;
    pendingActionId: string | undefined;
    pinned: Event[];
    pinnedIds: string[];
    visibleArtifact: Artifact | undefined;
  };
  selection: {
    hoveredItemId: string | undefined;
    selectedItemId: string | undefined;
  };
  sheet: {
    expanded: boolean;
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
  hoveredItemId: undefined,
  markPositions: new Map(),
  pendingCompose: undefined,
  registerDocument: unavailableMargin,
  replaceCompose: unavailableMargin,
  selectItem: unavailableMargin,
  selectedItemId: undefined,
  setHoveredItemId: unavailableMargin,
  setMarkPositions: unavailableMargin,
  settleCompose: unavailableMargin,
});

export function MarginProvider({ children }: { children: ReactNode }): ReactNode {
  const [documentBridge, setDocumentBridge] = useState<DocumentBridge>();
  const [focusRequest, setFocusRequest] = useState<{ markId: string; seq: number }>();
  const [hoveredItemId, setHoveredItemId] = useState<string>();
  const [markPositions, setMarkPositions] = useState<ReadonlyMap<string, number>>(() => new Map());
  const [pendingCompose, setPendingCompose] = useState<
    (MarkComposeRequest & { seq: number }) | undefined
  >();
  const [selectedItemId, setSelectedItemId] = useState<string>();
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
  const registerDocument = useCallback((bridge: DocumentBridge | undefined) => {
    setDocumentBridge(bridge);
  }, []);
  const value = useMemo<MarginContextValue>(
    () => ({
      composeForMark,
      documentBridge,
      focusItemForMark,
      focusRequest,
      hoveredItemId,
      markPositions,
      pendingCompose,
      registerDocument,
      replaceCompose,
      selectItem: setSelectedItemId,
      selectedItemId,
      setHoveredItemId,
      setMarkPositions,
      settleCompose,
    }),
    [
      composeForMark,
      documentBridge,
      focusItemForMark,
      focusRequest,
      hoveredItemId,
      markPositions,
      pendingCompose,
      registerDocument,
      replaceCompose,
      selectedItemId,
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
    markPositions,
    pendingCompose,
    selectItem,
    selectedItemId,
    setHoveredItemId,
    settleCompose,
    replaceCompose,
  } = useMargin();
  const { pathname } = useLocation();
  const route = parseIssuePath(pathname);
  const issueKey = route?.key;
  const routeArtifactSlug = route?.kind === "artifact" ? route.slug : undefined;
  const routeItemId = route?.kind === "ask" || route?.kind === "comment" ? route.id : undefined;
  const [tab, setTab] = useState<MarginTab>("comments");
  const [composer, setComposer] = useState<MarginComposer>();
  const [expandedIssueKey, setExpandedIssueKey] = useState<string>();
  const marginRef = useRef<HTMLElement>(null);
  const issue = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["issue", issueKey],
    queryFn: () => api.getIssue(issueKey ?? ""),
  });
  const visibleArtifact =
    routeArtifactSlug === undefined
      ? issue.data?.artifacts.find((artifact) => artifact.id === issue.data?.primary_artifact_id)
      : issue.data?.artifacts.find((artifact) => artifact.slug === routeArtifactSlug);
  const {
    actionErrorId,
    answeredAsksPending,
    asksPending,
    commentsError,
    commentsPending,
    commentRecords,
    items,
    marginItems,
    mutateItem,
    needsYou,
    pendingActionId,
    openAskCount,
    pinned,
    pinnedIds,
    retryAnsweredAsk,
    retryComments,
    retryItem,
  } = useMarginItems(issueKey, tab, visibleArtifact, markPositions);
  const isClosed = issue.data !== undefined && issue.data.closed_at !== null;
  const sheetExpanded = issueKey !== undefined && expandedIssueKey === issueKey;
  const toggleSheet = useCallback(
    (expanded?: boolean) => {
      const nextExpanded = expanded ?? !sheetExpanded;
      setExpandedIssueKey(nextExpanded ? issueKey : undefined);
    },
    [issueKey, sheetExpanded]
  );
  const closeComposer = useCallback(() => {
    setComposer(undefined);
    settleCompose("cancelled");
  }, [settleCompose]);
  const onComposerSaved = useCallback(() => {
    settleCompose("saved");
  }, [settleCompose]);
  const openComposer = useCallback(
    (kind: ComposerKind, anchor: ComposerAnchor | undefined, replyTo?: string) => {
      replaceCompose();
      setComposer({ anchor, kind, replyTo });
    },
    [replaceCompose]
  );

  useEffect(() => {
    if (pendingCompose === undefined) {
      return;
    }
    setTab("comments");
    setComposer({ anchor: pendingCompose.anchor, kind: pendingCompose.kind });
    if (issueKey !== undefined && window.matchMedia("(max-width: 1279px)").matches) {
      setExpandedIssueKey(issueKey);
    }
  }, [issueKey, pendingCompose]);
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
    if (routeItemId !== undefined && window.matchMedia("(max-width: 1279px)").matches) {
      setExpandedIssueKey(issueKey);
    }
  }, [issueKey, routeItemId]);

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
    selectItem(marginItemId(item));
    setTab("comments");
    if (issueKey !== undefined && window.matchMedia("(max-width: 1279px)").matches) {
      setExpandedIssueKey(issueKey);
    }
  }, [focusRequest, issueKey, marginItems, selectItem]);
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
      selectItem(id);
      const item = marginItems.find((candidate) => marginItemId(candidate) === id);
      const markId = item === undefined ? undefined : marginItemMarkId(item);
      if (markId !== undefined) {
        documentBridge?.focusMark(markId);
      }
    },
    [documentBridge, marginItems, selectItem]
  );
  const onReply = useCallback(
    (comment: Comment) => {
      openComposer("comment", undefined, threadRootId(commentRecords, comment));
    },
    [commentRecords, openComposer]
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
    selectItem,
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
      onReply,
      onRetryAction: retryItem,
      onRetryAnsweredAsk: retryAnsweredAsk,
      onRetryComments: retryComments,
      onRetryIssue: () => void issue.refetch(),
    },
    composer,
    items: {
      actionErrorId,
      answeredAsksPending,
      asksPending,
      comments: items,
      commentsError,
      commentsPending,
      marginRef,
      isClosed,
      issueError: issue.isError,
      issueKey,
      issuePending: issue.isPending,
      needsYou,
      onSelectCard,
      openAskCount,
      pendingActionId,
      pinned,
      pinnedIds,
      visibleArtifact,
    },
    selection: {
      hoveredItemId,
      selectedItemId,
    },
    sheet: {
      expanded: sheetExpanded,
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

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

import type { Anchor, Artifact, Comment, Event } from "../../api/types";
import type { MarginComposer } from "./CommentsTab";
import type { ComposerAnchor, ComposerKind } from "./Composer";
import { MarginSheet } from "./MarginSheet";
import type { MarginItem, MarginItemAction, MarginTab } from "./useMarginItems";
import { useMarginItems } from "./useMarginItems";
import { useMarginListeners } from "./useMarginListeners";

export interface MarginSelection extends ComposerAnchor {
  artifactId: string;
  canSuggest: boolean;
  rect: { bottom: number; left: number; right: number; top: number };
}

export interface MarginAnchor {
  anchor: Anchor;
  id: string;
}

interface MarginContextValue {
  anchors: MarginAnchor[];
  hoveredItemId: string | undefined;
  selectItem: (id: string) => void;
  selectedItemId: string | undefined;
  selection: MarginSelection | undefined;
  setAnchors: (anchors: MarginAnchor[]) => void;
  setHoveredItemId: (id: string | undefined) => void;
  setSelection: (selection: MarginSelection | undefined) => void;
}

export interface MarginSheetModel {
  actions: {
    closeComposer: () => void;
    onAction: (id: string, action: MarginItemAction) => void;
    onReply: (comment: Comment) => void;
    onRetryAction: () => void;
    onRetryAnsweredAsk: (() => void) | undefined;
    onRetryComments: () => void;
    onRetryIssue: () => void;
    onSelectionAction: (kind: ComposerKind, anchor: ComposerAnchor) => void;
  };
  composer: MarginComposer | undefined;
  items: {
    actionErrorId: string | undefined;
    answeredAsksPending: boolean;
    asksPending: boolean;
    comments: MarginItem[];
    commentsError: boolean;
    commentsPending: boolean;
    commentListRef: RefObject<HTMLDivElement | null>;
    isClosed: boolean;
    issueError: boolean;
    issueKey: string | undefined;
    issuePending: boolean;
    openAskCount: number;
    pendingActionId: string | undefined;
    pinned: Event[];
    pinnedIds: string[];
    visibleArtifact: Artifact | undefined;
  };
  selection: {
    hoveredItemId: string | undefined;
    selectedItemId: string | undefined;
    value: MarginSelection | undefined;
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

const noMargin = () => {};
const MarginContext = createContext<MarginContextValue>({
  anchors: [],
  hoveredItemId: undefined,
  selectItem: noMargin,
  selectedItemId: undefined,
  selection: undefined,
  setAnchors: noMargin,
  setHoveredItemId: noMargin,
  setSelection: noMargin,
});

export function MarginProvider({ children }: { children: ReactNode }): ReactNode {
  const [anchors, setAnchors] = useState<MarginAnchor[]>([]);
  const [hoveredItemId, setHoveredItemId] = useState<string>();
  const [selectedItemId, setSelectedItemId] = useState<string>();
  const [selection, setSelection] = useState<MarginSelection>();
  const value = useMemo<MarginContextValue>(
    () => ({
      anchors,
      hoveredItemId,
      selectItem: setSelectedItemId,
      selectedItemId,
      selection,
      setAnchors,
      setHoveredItemId,
      setSelection,
    }),
    [anchors, hoveredItemId, selectedItemId, selection]
  );

  return <MarginContext.Provider value={value}>{children}</MarginContext.Provider>;
}

export function useMargin(): MarginContextValue {
  return useContext(MarginContext);
}

function useMarginSheet(): MarginSheetModel {
  const {
    hoveredItemId,
    selectItem,
    selectedItemId,
    selection,
    setAnchors,
    setHoveredItemId,
    setSelection,
  } = useMargin();
  const [tab, setTab] = useState<MarginTab>("comments");
  const [composer, setComposer] = useState<MarginComposer>();
  const [expandedIssueKey, setExpandedIssueKey] = useState<string>();
  const commentListRef = useRef<HTMLDivElement>(null);
  const {
    actionErrorId,
    answeredAsksPending,
    asksPending,
    commentsError,
    commentsPending,
    decorationAnchors,
    isClosed,
    issueError,
    issueKey,
    issuePending,
    items,
    mutateItem,
    pendingActionId,
    openAskCount,
    pinned,
    pinnedIds,
    retryAnsweredAsk,
    retryComments,
    retryIssue,
    retryItem,
    routeArtifactSlug,
    routeItemId,
    visibleArtifact,
  } = useMarginItems(tab);
  const sheetExpanded = issueKey !== undefined && expandedIssueKey === issueKey;
  const toggleSheet = useCallback(
    (expanded?: boolean) => {
      const nextExpanded = expanded ?? !sheetExpanded;
      setExpandedIssueKey(nextExpanded ? issueKey : undefined);
    },
    [issueKey, sheetExpanded]
  );

  useEffect(() => {
    setAnchors(decorationAnchors);
  }, [decorationAnchors, setAnchors]);
  useEffect(() => {
    if (selection !== undefined && selection.artifactId !== visibleArtifact?.id) {
      setSelection(undefined);
    }
  }, [selection, setSelection, visibleArtifact?.id]);
  useEffect(() => {
    if (composer !== undefined && (isClosed || composer.anchor.artifact !== visibleArtifact?.id)) {
      setComposer(undefined);
    }
  }, [composer, isClosed, visibleArtifact?.id]);
  useEffect(() => {
    if (
      routeArtifactSlug === undefined ||
      visibleArtifact === undefined ||
      visibleArtifact.kind === "doc"
    ) {
      return;
    }
    setTab("artifacts");
    if (window.matchMedia("(max-width: 1279px)").matches) {
      setExpandedIssueKey(issueKey);
    }
  }, [issueKey, routeArtifactSlug, visibleArtifact]);
  useEffect(() => {
    if (routeItemId !== undefined && window.matchMedia("(max-width: 1279px)").matches) {
      setExpandedIssueKey(issueKey);
    }
  }, [issueKey, routeItemId]);

  useMarginListeners({
    items,
    list: commentListRef,
    routeItemId,
    selectItem,
    setHoveredItemId,
    setTab,
    sheetExpanded,
    tab,
    visibleArtifact,
  });

  const openComposer = useCallback(
    (kind: ComposerKind, anchor: ComposerAnchor, replyTo?: string) => {
      setComposer({ anchor, kind, replyTo });
      setSelection(undefined);
    },
    [setSelection]
  );
  const onAction = useCallback(
    (id: string, action: MarginItemAction) => {
      mutateItem({ id, kind: action });
    },
    [mutateItem]
  );
  const onReply = useCallback(
    (comment: Comment) => {
      if (comment.anchor !== null) {
        openComposer(
          "comment",
          {
            artifact: comment.anchor.artifact_id,
            from: comment.anchor.from,
            quote: comment.anchor.quote,
            to: comment.anchor.to,
          },
          comment.id
        );
      }
    },
    [openComposer]
  );
  const closeComposer = useCallback(() => {
    setComposer(undefined);
  }, []);

  return {
    actions: {
      closeComposer,
      onAction,
      onReply,
      onRetryAction: retryItem,
      onRetryAnsweredAsk: retryAnsweredAsk,
      onRetryComments: retryComments,
      onRetryIssue: retryIssue,
      onSelectionAction: openComposer,
    },
    composer,
    items: {
      actionErrorId,
      answeredAsksPending,
      asksPending,
      comments: items,
      commentsError,
      commentsPending,
      commentListRef,
      isClosed,
      issueError,
      issueKey,
      issuePending,
      openAskCount,
      pendingActionId,
      pinned,
      pinnedIds,
      visibleArtifact,
    },
    selection: {
      hoveredItemId,
      selectedItemId,
      value: selection,
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

interface MarginProps {
  ArtifactsTabSlot?: () => ReactNode;
}

export function Margin({ ArtifactsTabSlot }: MarginProps): ReactNode {
  const model = useMarginSheet();

  return <MarginSheet ArtifactsTabSlot={ArtifactsTabSlot} model={model} />;
}

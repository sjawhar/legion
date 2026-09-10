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
    onSelectionAction: (kind: ComposerKind, anchor: ComposerAnchor) => void;
  };
  composer: MarginComposer | undefined;
  items: {
    asksPending: boolean;
    comments: MarginItem[];
    commentsPending: boolean;
    commentListRef: RefObject<HTMLDivElement | null>;
    isClosed: boolean;
    issueKey: string | undefined;
    issuePending: boolean;
    openAskCount: number;
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
    asksPending,
    commentsPending,
    decorationAnchors,
    isClosed,
    issueKey,
    issuePending,
    items,
    mutateItem,
    openAskCount,
    pinned,
    pinnedIds,
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
    if (window.matchMedia("(max-width: 767px)").matches) {
      setExpandedIssueKey(issueKey);
    }
  }, [issueKey, routeArtifactSlug, visibleArtifact]);

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
      onSelectionAction: openComposer,
    },
    composer,
    items: {
      asksPending,
      comments: items,
      commentsPending,
      commentListRef,
      isClosed,
      issueKey,
      issuePending,
      openAskCount,
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

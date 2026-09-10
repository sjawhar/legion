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

import type { Artifact, Ask, Comment, Event } from "../../api/types";
import type { MarginComposer } from "./CommentsTab";
import type { ComposerAnchor, ComposerKind } from "./Composer";
import { MarginSheet } from "./MarginSheet";
import {
  type MarginItem,
  type MarginItemAction,
  type MarginTab,
  threadRootId,
  useMarginItems,
} from "./useMarginItems";
import { useMarginListeners } from "./useMarginListeners";

export interface MarginSelection extends ComposerAnchor {
  artifactId: string;
  canSuggest: boolean;
  rect: { bottom: number; left: number; right: number; top: number };
}

interface MarginContextValue {
  documentText: string;
  hoveredItemId: string | undefined;
  selectItem: (id: string) => void;
  selectedItemId: string | undefined;
  selection: MarginSelection | undefined;
  setDocumentText: (text: string) => void;
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
    marginRef: RefObject<HTMLElement | null>;
    isClosed: boolean;
    issueError: boolean;
    issueKey: string | undefined;
    issuePending: boolean;
    openAskCount: number;
    needsYou: Ask[];
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
  documentText: "",
  hoveredItemId: undefined,
  selectItem: noMargin,
  selectedItemId: undefined,
  selection: undefined,
  setDocumentText: noMargin,
  setHoveredItemId: noMargin,
  setSelection: noMargin,
});

export function MarginProvider({ children }: { children: ReactNode }): ReactNode {
  const [documentText, setDocumentText] = useState("");
  const [hoveredItemId, setHoveredItemId] = useState<string>();
  const [selectedItemId, setSelectedItemId] = useState<string>();
  const [selection, setSelection] = useState<MarginSelection>();
  const value = useMemo<MarginContextValue>(
    () => ({
      documentText,
      hoveredItemId,
      selectItem: setSelectedItemId,
      selectedItemId,
      selection,
      setDocumentText,
      setHoveredItemId,
      setSelection,
    }),
    [documentText, hoveredItemId, selectedItemId, selection]
  );

  return <MarginContext.Provider value={value}>{children}</MarginContext.Provider>;
}

export function useMargin(): MarginContextValue {
  return useContext(MarginContext);
}

function useMarginSheet(): MarginSheetModel {
  const {
    documentText,
    hoveredItemId,
    selectItem,
    selectedItemId,
    selection,
    setHoveredItemId,
    setSelection,
  } = useMargin();
  const [tab, setTab] = useState<MarginTab>("comments");
  const [composer, setComposer] = useState<MarginComposer>();
  const [expandedIssueKey, setExpandedIssueKey] = useState<string>();
  const marginRef = useRef<HTMLElement>(null);
  const {
    actionErrorId,
    answeredAsksPending,
    asksPending,
    commentsError,
    commentsPending,
    commentRecords,
    isClosed,
    issueError,
    issueKey,
    issuePending,
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
    retryIssue,
    retryItem,
    routeItemId,
    visibleArtifact,
  } = useMarginItems(tab, documentText);

  const sheetExpanded = issueKey !== undefined && expandedIssueKey === issueKey;
  const toggleSheet = useCallback(
    (expanded?: boolean) => {
      const nextExpanded = expanded ?? !sheetExpanded;
      setExpandedIssueKey(nextExpanded ? issueKey : undefined);
    },
    [issueKey, sheetExpanded]
  );

  useEffect(() => {
    if (selection !== undefined && selection.artifactId !== visibleArtifact?.id) {
      setSelection(undefined);
    }
  }, [selection, setSelection, visibleArtifact?.id]);
  useEffect(() => {
    if (
      composer !== undefined &&
      (isClosed ||
        (composer.anchor !== undefined && composer.anchor.artifact !== visibleArtifact?.id))
    ) {
      setComposer(undefined);
    }
  }, [composer, isClosed, visibleArtifact?.id]);
  useEffect(() => {
    if (routeItemId !== undefined && window.matchMedia("(max-width: 1279px)").matches) {
      setExpandedIssueKey(issueKey);
    }
  }, [issueKey, routeItemId]);

  useMarginListeners({
    items: marginItems,
    margin: marginRef,
    routeItemId,
    selectItem,
    setHoveredItemId,
    setTab,
    sheetExpanded,
    tab,
    visibleArtifact,
  });

  const openComposer = useCallback(
    (kind: ComposerKind, anchor: ComposerAnchor | undefined, replyTo?: string) => {
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
      openComposer("comment", undefined, threadRootId(commentRecords, comment));
    },
    [commentRecords, openComposer]
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
      marginRef,
      isClosed,
      issueError,
      issueKey,
      issuePending,
      openAskCount,
      needsYou,
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

export function Margin(): ReactNode {
  const model = useMarginSheet();

  return <MarginSheet model={model} />;
}

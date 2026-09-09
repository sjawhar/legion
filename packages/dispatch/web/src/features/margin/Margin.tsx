import { createContext, type ReactNode, useContext, useEffect, useMemo, useState } from "react";

import type { Anchor } from "../../api/types";
import type { MarginComposer } from "./CommentsTab";
import type { ComposerAnchor, ComposerKind } from "./Composer";
import { MarginSheet } from "./MarginSheet";
import type { MarginTab } from "./useMarginItems";
import { useMarginItems } from "./useMarginItems";

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

interface MarginProps {
  ArtifactsTabSlot?: () => ReactNode;
}

export function Margin({ ArtifactsTabSlot }: MarginProps): ReactNode {
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

  const openComposer = (kind: ComposerKind, anchor: ComposerAnchor, replyTo?: string) => {
    setComposer({ anchor, kind, replyTo });
    setSelection(undefined);
  };

  return (
    <MarginSheet
      ArtifactsTabSlot={ArtifactsTabSlot}
      asksPending={asksPending}
      commentsPending={commentsPending}
      composer={composer}
      hoveredItemId={hoveredItemId}
      isClosed={isClosed}
      issueKey={issueKey}
      issuePending={issuePending}
      items={items}
      onAction={(id, kind) => mutateItem({ id, kind })}
      onCloseComposer={() => setComposer(undefined)}
      onReply={(comment) => {
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
      }}
      onSelectionAction={openComposer}
      openAskCount={openAskCount}
      pinned={pinned}
      pinnedIds={pinnedIds}
      routeArtifactSlug={routeArtifactSlug}
      routeItemId={routeItemId}
      selectItem={selectItem}
      selectedItemId={selectedItemId}
      selection={selection}
      setHoveredItemId={setHoveredItemId}
      setTab={setTab}
      tab={tab}
      visibleArtifact={visibleArtifact}
    />
  );
}

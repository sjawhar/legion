import { itemFromSearch } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { whoAmIQuery } from "../../api/queries";
import type { UserState } from "../../api/types";
import { isRetractedAsk } from "../conversation/conversation-model";
import { useProjectArtifact } from "../document/useProjectArtifact";
import { eventItemId, stateForIssue } from "../issue/pins";
import {
  applyPinStateOperation,
  issueStateTransport,
  sharedIssueStateWrites,
} from "../issue/state-write-queue";
import { parseIssuePath, parseProjectPath } from "../refs/routes";
import { COMPACT_VIEWPORT_QUERY, PHONE_VIEWPORT_QUERY } from "../shell/useDialog";
import type { MarginComposer } from "./CommentsTab";
import type { MarginSheetModel } from "./MarginSheet";
import { useMargin } from "./margin-context";
import {
  type MarginItem,
  type MarginItemAction,
  type MarginTab,
  marginItemId,
  marginItemMarkId,
  threadMarkId,
  useMarginItems,
  useMarginOwner,
} from "./useMarginItems";
import { useMarginListeners } from "./useMarginListeners";

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

/**
 * Everything the margin's one rendered sheet needs: the owner and its route, the items, the
 * selection and composer state, and the listeners that hold a linked card in view. Lifted out of
 * `Margin.tsx` unchanged - `MarginProvider` owns the shared state above the router, this owns
 * what the sheet on screen does with it.
 */
export function useMarginSheet(): MarginSheetModel {
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
          ...issueStateTransport,
          onDrained: (key, next) => {
            queryClient.setQueryData<UserState>(["user-state"], (current) => ({
              ...current,
              [key]: next,
            }));
          },
          onError: () => {
            void queryClient.invalidateQueries({ queryKey: ["user-state"] });
          },
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
  // Every anchored and resolved thread by key: two callbacks and the sheet all resolve a key,
  // and rebuilding a combined array per call allocated on every mark click and card tap.
  const threadsByKey = useMemo(
    () => new Map([...threads, ...resolvedThreads].map((thread) => [thread.key, thread])),
    [resolvedThreads, threads]
  );
  // A compact viewport has no room beside the document, so anything that puts something new in
  // the margin - a composer, a document item link, a block filter, a focused mark - opens the
  // sheet over it. On a desktop the margin is already on screen and nothing opens.
  const openCompactSheet = useCallback(() => {
    if (ownerId !== undefined && window.matchMedia(COMPACT_VIEWPORT_QUERY).matches) {
      setExpandedOwnerId(ownerId);
    }
  }, [ownerId]);
  const selectedBlockFocus = useRef<string | undefined>(undefined);
  // `openPhoneThread`: a mark click or card tap opens the thread in the phone's dialog; a
  // route-driven selection (a comment or ask deep link) only highlights the card — on a phone
  // the link's destination is the Conversation turn, which a modal dialog would cover.
  const selectMarginItem = useCallback(
    (id: string, openPhoneThread = true) => {
      selectItem(id);
      const thread = threadsByKey.get(id);
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
    [owner, ownerId, selectItem, threadsByKey]
  );
  const onToggleThread = useCallback(
    (key: string) => {
      const thread = threadsByKey.get(key);
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
    [documentBridge, focusBlock, ownerId, selectItem, threadsByKey]
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
    openCompactSheet();
  }, [openCompactSheet, pendingCompose]);
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
    if (documentItemId !== undefined) {
      openCompactSheet();
    }
  }, [documentItemId, openCompactSheet]);
  useEffect(() => {
    if (blockFilterId === undefined) {
      return;
    }
    setTab("comments");
    openCompactSheet();
  }, [blockFilterId, openCompactSheet]);

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
    openCompactSheet();
  }, [focusRequest, marginItems, openCompactSheet, selectMarginItem]);
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
      thread: sheetThreadKey === undefined ? undefined : threadsByKey.get(sheetThreadKey),
      toggle: toggleSheet,
    },
    tab: {
      set: setTab,
      value: tab,
    },
  };
}

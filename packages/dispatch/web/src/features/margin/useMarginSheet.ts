import { itemFromSearch } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { primarySpec } from "../../api/issue-cache";
import { whoAmIQuery } from "../../api/queries";
import type { UserState } from "../../api/types";
import { useSending } from "../../hooks/useSending";
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
import { type MarginReply, type MarginReplyEntry, marginReplySendKey } from "./MarginReply";
import type { MarginSheetModel } from "./MarginSheet";
import { marginComposeSendKey, useMargin } from "./margin-context";
import {
  type MarginItem,
  type MarginItemAction,
  type MarginTab,
  marginItemId,
  marginItemMarkId,
  marginItemRecord,
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
    resumeCompose,
    retypeCompose,
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
  const [composers, setComposers] = useState<readonly MarginComposer[]>([]);
  const [expandedOwnerId, setExpandedOwnerId] = useState<string>();
  const [expandedThreadKey, setExpandedThreadKey] = useState<string>();
  const [editingCommentId, setEditingCommentId] = useState<string>();
  const [savingCommentEditId, setSavingCommentEditId] = useState<string>();
  const [sheetThreadKey, setSheetThreadKey] = useState<string>();
  const [showResolved, setShowResolved] = useState(false);
  // Threads whose reply holds a send of its own: each stays where it is (`useMarginItems`'s
  // `held`), whoever resolves it meanwhile.
  const [heldReplies, setHeldReplies] = useState<ReadonlySet<string>>(() => new Set());
  const onReplyHolding = useCallback((key: string, holding: boolean) => {
    setHeldReplies((current) => {
      if (current.has(key) === holding) return current;
      const next = new Set(current);
      if (holding) next.add(key);
      else next.delete(key);
      return next;
    });
  }, []);
  // Every thread's reply composer the margin keeps (`MarginReply`), from the first time a card
  // shows the thread, and the element each renders into, which `attachReply` hands a card before
  // the reply is in state. A reply goes when the reader leaves its document, unless it holds a
  // send of its own then, and with a phone thread's Back.
  const [replies, setReplies] = useState<readonly MarginReply[]>([]);
  const replyNodes = useRef(new Map<string, HTMLElement>());
  const dropReplies = useCallback((keys: ReadonlySet<string>) => {
    for (const key of keys) replyNodes.current.delete(key);
    setReplies((current) => current.filter((reply) => !keys.has(reply.key)));
  }, []);
  // The phone margin thread's reply names its send, so the thread's Back and Escape hold while it
  // is out, until its deadline. No thread has the empty key, so with none open nothing matches.
  const sheetReplyKey = useMemo(() => marginReplySendKey(sheetThreadKey ?? ""), [sheetThreadKey]);
  const { sending: sheetReplySending, sendingNow: sheetReplySendingNow } = useSending(
    sheetReplyKey,
    { untilDeadline: true }
  );
  const closeThread = useCallback(() => {
    if (sheetReplySendingNow()) return;
    setSheetThreadKey(undefined);
    // Back drops the thread's reply, its draft and a refusal with it, as a Conversation phone
    // thread's does - unless its send is still out past the deadline, when the reply keeps the
    // send, and its answer, for the thread's return.
    if (
      sheetThreadKey !== undefined &&
      queryClient.isMutating({ mutationKey: marginReplySendKey(sheetThreadKey) }) === 0
    ) {
      dropReplies(new Set([sheetThreadKey]));
    }
  }, [dropReplies, queryClient, sheetReplySendingNow, sheetThreadKey]);
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
    actionFailure,
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
  } = useMarginItems(owner, tab, visibleArtifact, markPlacements, blockPlacements, blockFilterId, {
    held: heldReplies,
  });
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
        closeThread();
      }
    },
    [closeThread, ownerId, sheetExpanded]
  );
  // A card showing a thread takes the thread's reply element into its slot, and the margin keeps
  // the reply from then on, sending as the owner the thread was shown under.
  const visibleArtifactId = visibleArtifact?.id;
  const attachReply = useCallback(
    (key: string, slot: HTMLElement) => {
      let node = replyNodes.current.get(key);
      if (node === undefined) {
        node = document.createElement("div");
        node.className = "contents";
        replyNodes.current.set(key, node);
      }
      const element = node;
      slot.appendChild(element);
      if (owner !== undefined && visibleArtifactId !== undefined) {
        setReplies((current) =>
          current.some((reply) => reply.key === key)
            ? current
            : [...current, { artifact: visibleArtifactId, key, node: element, owner }]
        );
      }
      return () => {
        if (element.parentNode === slot) slot.removeChild(element);
      };
    },
    [owner, visibleArtifactId]
  );
  // A reply goes once the reader has left its document and no card shows it - unless it holds a
  // send of its own, out or refused, which it keeps for their return, as a held selection-bar
  // composer does.
  useEffect(() => {
    const left = new Set(
      replies
        .filter(
          (reply) =>
            reply.artifact !== visibleArtifactId &&
            !reply.node.isConnected &&
            !heldReplies.has(reply.key) &&
            queryClient.isMutating({ mutationKey: marginReplySendKey(reply.key) }) === 0
        )
        .map((reply) => reply.key)
    );
    if (left.size > 0) dropReplies(left);
  }, [dropReplies, heldReplies, queryClient, replies, visibleArtifactId]);
  // A reply's Cancel reply and close close the thread it answers: a phone thread as its Back does,
  // which drops the reply, and an expanded card by collapsing it.
  const closeReply = useCallback(
    (key: string) => {
      if (key === sheetThreadKey && window.matchMedia(PHONE_VIEWPORT_QUERY).matches) closeThread();
      else onToggleThread(key);
    },
    [closeThread, onToggleThread, sheetThreadKey]
  );
  const replyEntries = useMemo<readonly MarginReplyEntry[]>(
    () =>
      replies.map((reply) => {
        const shown = reply.artifact === visibleArtifactId;
        const accepted = shown
          ? threadsByKey.get(reply.key)?.root.comment.suggestion?.accepted
          : undefined;
        return { ...reply, finishing: accepted !== undefined && accepted !== null, shown };
      }),
    [replies, threadsByKey, visibleArtifactId]
  );
  // A composer's close and save name the compose they belong to, so a send of a compose the reader
  // left behind, landing after a newer one opened, closes and settles nothing of the newer one.
  const closeComposer = useCallback(
    (seq: number) => {
      setComposers((current) => current.filter((entry) => entry.seq !== seq));
      settleCompose("cancelled", seq);
    },
    [settleCompose]
  );
  const onComposerSaved = useCallback(
    (seq: number) => {
      settleCompose("saved", seq);
    },
    [settleCompose]
  );

  // A compose the margin publishes is shown: a new one, the open one brought back because a newer
  // selection-bar action had to wait for its send (`turnedAway`), and a held one the reader came
  // back to. A document has one composer, so a newer compose there takes the composer the reader
  // has there, which keeps what it holds.
  useEffect(() => {
    if (pendingCompose === undefined || owner === undefined) {
      return;
    }
    const { anchor, kind, seq } = pendingCompose;
    const turnedAway = pendingCompose.turnedAway === true;
    setTab("comments");
    setComposers((current) => {
      const kept = current.find((entry) => entry.anchor.artifact === anchor.artifact);
      if (
        kept?.seq === seq &&
        kept.anchor === anchor &&
        kept.kind === kind &&
        kept.turnedAway === turnedAway
      ) {
        return current;
      }
      const next: MarginComposer =
        kept?.seq === seq
          ? { ...kept, anchor, kind, turnedAway }
          : { anchor, held: false, kind, owner, seq, turnedAway };
      return kept === undefined
        ? [...current, next]
        : current.map((entry) => (entry === kept ? next : entry));
    });
    openCompactSheet();
  }, [openCompactSheet, owner, pendingCompose]);
  // A composer ends unsaved when the reader leaves its document, and when its issue closes - unless
  // its send is out then. That composer is held: it stays until the send lands or the reader
  // discards the refusal it hands back, hidden while its document is not the one open, and the
  // open compose again whenever it is (`resumeCompose`), so a newer selection-bar action there
  // waits on its send as on any open compose. On a closed issue it shows only that send and then
  // its outcome (`closed` on `MentionComposer`).
  useEffect(() => {
    for (const entry of composers) {
      const shown = entry.anchor.artifact === visibleArtifact?.id;
      if (!entry.held && (!shown || isClosed)) {
        if (queryClient.isMutating({ mutationKey: marginComposeSendKey(entry.seq) }) === 0) {
          closeComposer(entry.seq);
          continue;
        }
        setComposers((current) =>
          current.map((kept) => (kept.seq === entry.seq ? { ...kept, held: true } : kept))
        );
      }
      if (!shown) {
        settleCompose("left", entry.seq);
      } else if (entry.held) {
        resumeCompose(entry);
      }
    }
  }, [
    closeComposer,
    composers,
    isClosed,
    queryClient,
    resumeCompose,
    settleCompose,
    visibleArtifact?.id,
  ]);
  const shownComposer = useMemo(
    () => composers.find((entry) => entry.anchor.artifact === visibleArtifact?.id),
    [composers, visibleArtifact?.id]
  );
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
      .map((item) => marginItemRecord(item).anchor)
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
    const anchor = item === undefined ? undefined : marginItemRecord(item).anchor;
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
    composer: shownComposer,
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
      onComposerKindChange: retypeCompose,
      onEdit,
      onUnpin: unpin,
      onRetryAction: retryItem,
      onRetryAnsweredAsk: retryAnsweredAsk,
      onRetryComments: retryComments,
      onRetryIssue: () => void issue.refetch(),
      onToggleResolved: () => setShowResolved((current) => !current),
      onToggleThread,
    },
    composers,
    items: {
      actionFailure,
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
    replies: {
      attach: attachReply,
      close: closeReply,
      entries: replyEntries,
      onHolding: onReplyHolding,
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
      closeThread,
      expanded: sheetExpanded,
      replySending: sheetReplySending,
      thread: sheetThreadKey === undefined ? undefined : threadsByKey.get(sheetThreadKey),
      toggle: toggleSheet,
    },
    tab: {
      set: setTab,
      value: tab,
    },
  };
}

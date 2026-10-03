import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo, useState } from "react";
import { useLocation } from "react-router-dom";

import { api } from "../../api/client";
import { inboxQuery, userStateQuery } from "../../api/queries";
import type { Anchor, Artifact, Ask, Comment, Event } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import { useProjectArtifact } from "../document/useProjectArtifact";
import { pinnedEventIds } from "../issue/pins";
import { parseIssuePath, parseProjectPath } from "../refs/routes";
import { useAnsweredAsks } from "./useAnsweredAsks";
import { type CommentAction, useCommentActionQueue } from "./useCommentActionQueue";

export type MarginTab = "comments" | "pinned";

export type MarginOwner =
  | { kind: "issue"; key: string }
  | { kind: "document"; artifactId: string; project: string; slug: string };

/** The margin's owner, stable while the route and its artifact are: a fresh object here gave
 *  every `owner`-keyed memo, callback and effect in the margin a new identity each render. */
export function useMarginOwner(): MarginOwner | undefined {
  const { pathname, search } = useLocation();
  const issueRoute = parseIssuePath(pathname, search);
  const projectRoute = parseProjectPath(pathname, search);
  const document = projectRoute?.kind === "document" ? projectRoute : undefined;
  const artifact = useProjectArtifact(document);
  const issueKey = issueRoute?.key;
  const artifactId = artifact.data?.id;
  const project = document?.project;
  const slug = document?.slug;

  return useMemo(() => {
    if (issueKey !== undefined) {
      return { key: issueKey, kind: "issue" };
    }
    if (project !== undefined && slug !== undefined && artifactId !== undefined) {
      return { artifactId, kind: "document", project, slug };
    }
    return undefined;
  }, [artifactId, issueKey, project, slug]);
}
export type MarginItemAction = CommentAction;
export type MarginItem =
  | { ask: Ask; kind: "ask" }
  | { comment: ThreadComment; kind: "comment"; threadRootId: string };

export interface MarkPlacement {
  pos: number;
  top: number;
}

/**
 * A comment as a thread renders it. The margin builds threads from read rows (`Comment`) and the
 * Conversation from comment event payloads (`CommentEventPayload`), which omit `mentions` and
 * `deliveries` on events recorded before 2026-09-18; no thread renderer reads either field.
 */
export type ThreadComment = Omit<Comment, "mentions" | "deliveries">;

export interface Thread {
  anchor: Anchor | null;
  key: string;
  lastReplyAt: string | undefined;
  replies: ThreadComment[];
  resolved: boolean;
  root: { comment: ThreadComment; kind: "comment" };
}

const pinnedEventBatchSize = 50;

type IssueEventsFetcher = (issueKey: string, options: { ids: string[] }) => Promise<Event[]>;

export async function fetchPinnedEvents(
  listIssueEvents: IssueEventsFetcher,
  issueKey: string,
  ids: string[]
): Promise<Event[]> {
  const batches: string[][] = [];
  for (let index = 0; index < ids.length; index += pinnedEventBatchSize) {
    batches.push(ids.slice(index, index + pinnedEventBatchSize));
  }
  const events = await Promise.all(
    batches.map((batch) => listIssueEvents(issueKey, { ids: batch }))
  );
  return events.flat().sort((left, right) => left.seq - right.seq);
}

// A comment that replies directly to an ask (`ask_id` set), or transitively replies to one
// of those replies, belongs to that ask's own thread — AskCard already renders it via
// AskThread. Without this exclusion it would also surface here as a standalone root
// comment, rendering the same reply twice.
function withoutAskThreadReplies(comments: Comment[]): Comment[] {
  const askThreadIds = new Set(
    comments.filter((comment) => comment.ask_id !== null).map((comment) => comment.id)
  );
  let grew = true;
  while (grew) {
    grew = false;
    for (const comment of comments) {
      if (
        !askThreadIds.has(comment.id) &&
        comment.reply_to !== null &&
        askThreadIds.has(comment.reply_to)
      ) {
        askThreadIds.add(comment.id);
        grew = true;
      }
    }
  }
  return comments.filter((comment) => !askThreadIds.has(comment.id));
}
/**
 * Comments whose thread root is anchored to `artifactId`: the roots themselves and every reply
 * chained to one.
 */
export function anchoredThreadComments(
  comments: Comment[],
  artifactId: string | undefined
): Comment[] {
  if (artifactId === undefined) {
    return [];
  }
  const byId = new Map(comments.map((comment) => [comment.id, comment]));
  return comments.filter((comment) => rootOf(byId, comment).anchor?.artifact_id === artifactId);
}

/** The comment at the top of `comment`'s reply chain: the first with no `reply_to`, or the last
 *  one present when a parent is missing. A cyclic chain stops at the first repeated comment. */
function rootOf(byId: ReadonlyMap<string, Comment>, comment: Comment): Comment {
  const visited = new Set<string>();
  let root = comment;
  while (root.reply_to !== null && !visited.has(root.id)) {
    visited.add(root.id);
    const parent = byId.get(root.reply_to);
    if (parent === undefined) {
      break;
    }
    root = parent;
  }
  return root;
}

function commentThreads(comments: Comment[]): Thread[] {
  const byId = new Map(comments.map((comment) => [comment.id, comment]));
  const threads = new Map<string, { replies: Comment[]; root: Comment }>();
  for (const comment of comments) {
    const rootId = rootOf(byId, comment).id;
    const root = byId.get(rootId);
    if (root === undefined) {
      continue;
    }
    const entry = threads.get(rootId) ?? { replies: [], root };
    if (comment.id !== rootId) {
      entry.replies.push(comment);
    }
    threads.set(rootId, entry);
  }
  return [...threads.values()].map(({ replies, root }) => {
    replies.sort(
      (left, right) =>
        compareTimestamps(left.created_at, right.created_at) || left.id.localeCompare(right.id)
    );
    return {
      anchor: root.anchor,
      key: root.id,
      lastReplyAt: replies.at(-1)?.created_at,
      replies,
      resolved: root.resolved || (root.suggestion !== null && root.suggestion.accepted !== null),
      root: { comment: root, kind: "comment" },
    };
  });
}

/** The ask or comment a card shows. */
export function marginItemRecord(item: MarginItem): Ask | ThreadComment {
  return item.kind === "ask" ? item.ask : item.comment;
}

export function marginItemId(item: MarginItem): string {
  return marginItemRecord(item).id;
}

export function marginItemMarkId(item: MarginItem): string | undefined {
  return marginItemRecord(item).anchor?.mark_id;
}

export function threadMarkId(thread: Thread): string | undefined {
  return thread.anchor?.mark_id;
}

function anchorPlacement(
  anchor: Anchor | null,
  markPlacements: ReadonlyMap<string, MarkPlacement>,
  blockPlacements: ReadonlyMap<string, MarkPlacement>
): MarkPlacement | undefined {
  if (anchor === null) {
    return undefined;
  }
  if (anchor.orphaned) {
    return typeof anchor.block_id === "string" ? blockPlacements.get(anchor.block_id) : undefined;
  }
  return markPlacements.get(anchor.mark_id);
}

function isInBlock(anchor: Anchor | null, blockFilterId: string | undefined): boolean {
  return blockFilterId === undefined || anchor?.block_id === blockFilterId;
}

/** Document order for anchored cards: placed ones by position, unplaced ones after them, and
 *  among the unplaced the newest first, by time and then by id. */
function byPlacementThenNewest(
  left: Ask | ThreadComment,
  right: Ask | ThreadComment,
  markPlacements: ReadonlyMap<string, MarkPlacement>,
  blockPlacements: ReadonlyMap<string, MarkPlacement>
): number {
  const leftPlacement = anchorPlacement(left.anchor, markPlacements, blockPlacements);
  const rightPlacement = anchorPlacement(right.anchor, markPlacements, blockPlacements);
  if (leftPlacement !== undefined && rightPlacement !== undefined) {
    return leftPlacement.pos - rightPlacement.pos;
  }
  if (leftPlacement !== undefined || rightPlacement !== undefined) {
    return leftPlacement === undefined ? 1 : -1;
  }
  return compareTimestamps(right.created_at, left.created_at) || left.id.localeCompare(right.id);
}

const NO_THREADS: ReadonlySet<string> = new Set();
const NO_PLACEMENTS: ReadonlyMap<string, boolean> = new Map();

export function useMarginItems(
  owner: MarginOwner | undefined,
  tab: MarginTab,
  visibleArtifact: Artifact | undefined,
  markPlacements: ReadonlyMap<string, MarkPlacement>,
  blockPlacements: ReadonlyMap<string, MarkPlacement>,
  blockFilterId: string | undefined,
  {
    held = NO_THREADS,
  }: {
    /** Threads whose card's reply holds a send of its own - out, or refused - each of which stays
     *  in the list it was in, open or resolved, whoever resolves or reopens it meanwhile: moving
     *  it between them would remount its card, and the composer and what it holds with it. */
    readonly held?: ReadonlySet<string>;
  } = {}
) {
  const queryClient = useQueryClient();
  const issueKey = owner?.kind === "issue" ? owner.key : undefined;
  const commentsQueryKey =
    owner?.kind === "issue"
      ? ["comments", owner.key]
      : owner?.kind === "document"
        ? ["artifact", owner.artifactId, "comments"]
        : ["comments", undefined];
  const asks = useQuery(inboxQuery());
  const inboxOpenAsks = useMemo(
    () =>
      (asks.data ?? []).filter(
        (ask) =>
          ask.state === "open" &&
          (owner?.kind === "issue"
            ? ask.issue_key === owner.key
            : owner?.kind === "document"
              ? ask.artifact_id === owner.artifactId
              : false)
      ),
    [asks.data, owner]
  );
  const comments = useQuery({
    enabled: owner !== undefined,
    queryKey: commentsQueryKey,
    queryFn: () => {
      if (owner === undefined) {
        throw new Error("Margin comments require an owner.");
      }
      return owner.kind === "document"
        ? api.listArtifactComments(owner.artifactId)
        : api.listComments(owner.key);
    },
  });
  const userState = useQuery({ ...userStateQuery(), enabled: owner?.kind === "issue" });
  const pinnedIds =
    owner?.kind === "issue" ? pinnedEventIds(userState.data?.[owner.key]?.dismissed ?? []) : [];
  const pinned = useQuery({
    enabled: owner?.kind === "issue" && tab === "pinned" && pinnedIds.length > 0,
    queryKey: ["events", issueKey, "margin-pinned", pinnedIds],
    queryFn: () => {
      if (owner?.kind !== "issue") {
        throw new Error("Pinned margin events require an issue owner.");
      }
      return fetchPinnedEvents(api.getIssueEvents.bind(api), owner.key, pinnedIds);
    },
  });
  const answeredAsks = useAnsweredAsks(owner, visibleArtifact?.id);
  const needsYou = useMemo(() => {
    const openAsks = new Map<string, Ask>(inboxOpenAsks.map((ask) => [ask.id, ask]));
    for (const ask of answeredAsks.asks) {
      if (ask.state === "open") {
        openAsks.set(ask.id, ask);
      }
    }
    return [...openAsks.values()].filter((ask) => isInBlock(ask.anchor, blockFilterId));
  }, [answeredAsks.asks, blockFilterId, inboxOpenAsks]);
  const openAskCount = needsYou.length;
  const anchoredAsks = useMemo(
    () =>
      answeredAsks.asks.filter(
        (ask) => ask.state !== "open" && isInBlock(ask.anchor, blockFilterId)
      ),
    [answeredAsks.asks, blockFilterId]
  );
  /** Issue-level (unanchored) comments belong to the Conversation tab; a standalone document
   * has no Conversation, so its margin also lists document-level threads without a mark.
   * `anchoredThreadComments` returns a fresh array, so it has to be derived inside the memo:
   * outside it, every render gave the whole item pipeline below a new identity. */
  const ownerKind = owner?.kind;
  const allThreads = useMemo(() => {
    const visibleComments =
      ownerKind === "document"
        ? (comments.data ?? [])
        : anchoredThreadComments(comments.data ?? [], visibleArtifact?.id);
    return commentThreads(withoutAskThreadReplies(visibleComments)).filter((thread) =>
      isInBlock(thread.anchor, blockFilterId)
    );
  }, [blockFilterId, comments.data, ownerKind, visibleArtifact?.id]);
  const compareThreads = useCallback(
    (left: Thread, right: Thread) =>
      byPlacementThenNewest(left.root.comment, right.root.comment, markPlacements, blockPlacements),
    [blockPlacements, markPlacements]
  );
  const sortedThreads = useMemo(
    () => [...allThreads].sort(compareThreads),
    [allThreads, compareThreads]
  );
  // Whether each held thread sits with the resolved ones: where it was when its hold began. Kept
  // across renders, and set during render (React's pattern for state derived from props), so the
  // render that first holds a thread already places it.
  const [heldPlacement, setHeldPlacement] = useState(NO_PLACEMENTS);
  const placement = useMemo(() => {
    const next = new Map<string, boolean>();
    for (const thread of sortedThreads) {
      if (held.has(thread.key)) {
        next.set(thread.key, heldPlacement.get(thread.key) ?? thread.resolved);
      }
    }
    return next;
  }, [held, heldPlacement, sortedThreads]);
  if (
    placement.size !== heldPlacement.size ||
    [...placement].some(([key, resolved]) => heldPlacement.get(key) !== resolved)
  ) {
    setHeldPlacement(placement);
  }
  const threads = useMemo(
    () => sortedThreads.filter((thread) => !(placement.get(thread.key) ?? thread.resolved)),
    [placement, sortedThreads]
  );
  const resolvedThreads = useMemo(
    () => sortedThreads.filter((thread) => placement.get(thread.key) ?? thread.resolved),
    [placement, sortedThreads]
  );
  const items = useMemo<MarginItem[]>(() => {
    const commentItems = sortedThreads.map(({ root }) => ({
      comment: root.comment,
      kind: "comment" as const,
      threadRootId: root.comment.id,
    }));
    return [...anchoredAsks.map((ask) => ({ ask, kind: "ask" as const })), ...commentItems].sort(
      (left, right) =>
        byPlacementThenNewest(
          marginItemRecord(left),
          marginItemRecord(right),
          markPlacements,
          blockPlacements
        )
    );
  }, [anchoredAsks, blockPlacements, markPlacements, sortedThreads]);
  const marginItems = useMemo<MarginItem[]>(
    () => [...needsYou.map((ask) => ({ ask, kind: "ask" as const })), ...items],
    [items, needsYou]
  );
  const { actionFailure, mutateItem, pendingActionIds, retryItem } = useCommentActionQueue<{
    previous: Comment[] | undefined;
  }>({
    onError: (_error, _input, context) => {
      queryClient.setQueryData(commentsQueryKey, context?.previous);
      void queryClient.invalidateQueries({ queryKey: commentsQueryKey });
    },
    onMutate: async ({ id, kind }) => {
      await queryClient.cancelQueries({ queryKey: commentsQueryKey });
      const previous = queryClient.getQueryData<Comment[]>(commentsQueryKey);
      queryClient.setQueryData<Comment[]>(commentsQueryKey, (current) =>
        current?.map((comment) => {
          if (comment.id !== id) {
            return comment;
          }
          if (kind === "reopen") {
            return { ...comment, resolved: false, resolved_at: null, resolved_by: null };
          }
          if (kind === "resolve") {
            return { ...comment, resolved: true };
          }
          return comment.suggestion === null
            ? comment
            : {
                ...comment,
                resolved: true,
                suggestion: { ...comment.suggestion, accepted: kind === "accept" },
              };
        })
      );
      return { previous };
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: commentsQueryKey });
      if (owner?.kind === "issue") {
        void queryClient.invalidateQueries({ queryKey: ["issue", owner.key] });
      } else if (owner?.kind === "document") {
        void queryClient.invalidateQueries({ queryKey: ["artifact", owner.artifactId] });
      }
    },
  });
  const edit = useMutation({
    mutationFn: ({ body, id }: { body: string; id: string }) => api.editComment(id, { body }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: commentsQueryKey });
    },
  });

  const answeredAskError = answeredAsks.error;
  const retryAnsweredAsk =
    answeredAskError === undefined ? undefined : () => void answeredAskError.refetch();

  return {
    actionFailure,
    answeredAsksPending: answeredAsks.pending,
    asksPending: asks.isPending,
    commentsError: comments.isError,
    commentsPending: comments.isPending,
    commentRecords: comments.data ?? [],
    editComment: (id: string, body: string) => edit.mutateAsync({ body, id }),
    items,
    marginItems,
    needsYou,
    mutateItem,
    openAskCount,
    pendingActionIds,
    pinned: pinnedIds.length === 0 ? [] : (pinned.data ?? []),
    pinnedIds,
    resolvedThreads,
    isClosed: owner?.kind === "document" ? false : undefined,
    retryAnsweredAsk,
    retryComments: () => void comments.refetch(),
    retryItem,
    threads,
  };
}

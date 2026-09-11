import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import { useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { Anchor, Artifact, Ask, Comment, Event } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { pinnedEventIds } from "../issue/pins";
import { parseIssuePath, parseProjectPath } from "../refs/routes";
import { useAnsweredAsks } from "./useAnsweredAsks";

export type MarginTab = "comments" | "pinned";

export type MarginOwner =
  | { kind: "issue"; key: string }
  | { kind: "document"; artifactId: string; project: string; slug: string };

export function useMarginOwner(): MarginOwner | undefined {
  const { pathname, search } = useLocation();
  const issueRoute = parseIssuePath(pathname, search);
  const projectRoute = parseProjectPath(pathname, search);
  const document = projectRoute?.kind === "document" ? projectRoute : undefined;
  const artifact = useQuery({
    enabled: document !== undefined,
    queryKey: ["artifact-ref", `${document?.project}/${document?.slug}`],
    queryFn: () => {
      if (document === undefined) {
        throw new Error("Project document query requires a document route.");
      }
      return api.getProjectArtifact(document.project, document.slug);
    },
  });

  if (issueRoute !== undefined) {
    return { key: issueRoute.key, kind: "issue" };
  }
  if (document !== undefined && artifact.data !== undefined) {
    return {
      artifactId: artifact.data.id,
      kind: "document",
      project: document.project,
      slug: document.slug,
    };
  }
  return undefined;
}
export type MarginItemAction = "accept" | "reject" | "resolve" | "reopen";
export type MarginItem =
  | { ask: Ask; kind: "ask" }
  | { comment: Comment; kind: "comment"; threadRootId: string };

export interface MarkPlacement {
  pos: number;
  top: number;
}

export interface Thread {
  anchor: Anchor | null;
  key: string;
  lastReplyAt: string | undefined;
  replies: Comment[];
  resolved: boolean;
  root: { comment: Comment; kind: "comment" };
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
  const byId = new Map(comments.map((comment) => [comment.id, comment]));
  return comments.filter((comment) => {
    let root = comment;
    while (root.reply_to !== null) {
      const parent = byId.get(root.reply_to);
      if (parent === undefined) {
        break;
      }
      root = parent;
    }
    return root.anchor?.artifact_id === artifactId;
  });
}

export function threadRootId(comments: readonly Comment[], comment: Comment): string {
  const byID = new Map(comments.map((candidate) => [candidate.id, candidate]));
  const visited = new Set<string>();
  let root = comment;
  while (root.reply_to !== null && !visited.has(root.id)) {
    visited.add(root.id);
    const parent = byID.get(root.reply_to);
    if (parent === undefined) {
      break;
    }
    root = parent;
  }
  return root.id;
}

function commentThreads(comments: Comment[]): Thread[] {
  const byId = new Map(comments.map((comment) => [comment.id, comment]));
  const threads = new Map<string, { replies: Comment[]; root: Comment }>();
  for (const comment of comments) {
    const rootId = threadRootId(comments, comment);
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
    replies.sort((left, right) => left.created_at.localeCompare(right.created_at));
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

export function marginItemId(item: MarginItem): string {
  return item.kind === "ask" ? item.ask.id : item.comment.id;
}

export function marginItemMarkId(item: MarginItem): string | undefined {
  return item.kind === "ask" ? item.ask.anchor?.mark_id : item.comment.anchor?.mark_id;
}

export function threadMarkId(thread: Thread): string | undefined {
  return thread.anchor?.mark_id;
}

export function useMarginItems(
  owner: MarginOwner | undefined,
  tab: MarginTab,
  visibleArtifact: Artifact | undefined,
  markPlacements: ReadonlyMap<string, MarkPlacement>
) {
  const queryClient = useQueryClient();
  const issueKey = owner?.kind === "issue" ? owner.key : undefined;
  const commentsQueryKey =
    owner?.kind === "issue"
      ? ["comments", owner.key]
      : owner?.kind === "document"
        ? ["artifact", owner.artifactId, "comments"]
        : ["comments", undefined];
  const asks = useQuery({ queryKey: ["inbox"], queryFn: () => api.getInbox() });
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
  const userState = useQuery({
    enabled: owner?.kind === "issue",
    queryKey: ["user-state"],
    queryFn: () => api.getMyState(),
  });
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
    const openAsks = new Map(inboxOpenAsks.map((ask) => [ask.id, ask]));
    for (const ask of answeredAsks.asks) {
      if (ask.state === "open") {
        openAsks.set(ask.id, ask);
      }
    }
    return [...openAsks.values()];
  }, [answeredAsks.asks, inboxOpenAsks]);
  const openAskCount = needsYou.length;
  const anchoredAsks = useMemo(
    () => answeredAsks.asks.filter((ask) => ask.state !== "open"),
    [answeredAsks.asks]
  );
  /** Issue-level (unanchored) comments belong to the Conversation tab; the margin shows document-anchored review. */
  const allThreads = useMemo(
    () =>
      commentThreads(
        withoutAskThreadReplies(anchoredThreadComments(comments.data ?? [], visibleArtifact?.id))
      ),
    [comments.data, visibleArtifact?.id]
  );
  const compareThreads = useCallback(
    (left: Thread, right: Thread) => {
      const leftMarkId = threadMarkId(left);
      const rightMarkId = threadMarkId(right);
      const leftPlacement = leftMarkId === undefined ? undefined : markPlacements.get(leftMarkId);
      const rightPlacement =
        rightMarkId === undefined ? undefined : markPlacements.get(rightMarkId);
      if (leftPlacement !== undefined && rightPlacement !== undefined) {
        return leftPlacement.pos - rightPlacement.pos;
      }
      if (leftPlacement !== undefined || rightPlacement !== undefined) {
        return leftPlacement === undefined ? 1 : -1;
      }
      return right.root.comment.created_at.localeCompare(left.root.comment.created_at);
    },
    [markPlacements]
  );
  const sortedThreads = useMemo(
    () => [...allThreads].sort(compareThreads),
    [allThreads, compareThreads]
  );
  const threads = useMemo(
    () => sortedThreads.filter((thread) => !thread.resolved),
    [sortedThreads]
  );
  const resolvedThreads = useMemo(
    () => sortedThreads.filter((thread) => thread.resolved),
    [sortedThreads]
  );
  const items = useMemo<MarginItem[]>(() => {
    const commentItems = sortedThreads.map(({ root }) => ({
      comment: root.comment,
      kind: "comment" as const,
      threadRootId: root.comment.id,
    }));
    return [...anchoredAsks.map((ask) => ({ ask, kind: "ask" as const })), ...commentItems].sort(
      (left, right) => {
        const leftMarkId = marginItemMarkId(left);
        const rightMarkId = marginItemMarkId(right);
        const leftPlacement = leftMarkId === undefined ? undefined : markPlacements.get(leftMarkId);
        const rightPlacement =
          rightMarkId === undefined ? undefined : markPlacements.get(rightMarkId);
        if (leftPlacement !== undefined && rightPlacement !== undefined) {
          return leftPlacement.pos - rightPlacement.pos;
        }
        if (leftPlacement !== undefined || rightPlacement !== undefined) {
          return leftPlacement === undefined ? 1 : -1;
        }
        return (
          right.kind === "ask" ? right.ask.created_at : right.comment.created_at
        ).localeCompare(left.kind === "ask" ? left.ask.created_at : left.comment.created_at);
      }
    );
  }, [anchoredAsks, markPlacements, sortedThreads]);
  const marginItems = useMemo<MarginItem[]>(
    () => [...needsYou.map((ask) => ({ ask, kind: "ask" as const })), ...items],
    [items, needsYou]
  );
  const actionGuard = useSubmitGuard();
  const action = useMutation({
    mutationFn: ({ id, kind }: { id: string; kind: MarginItemAction }) => {
      if (kind === "accept") {
        return api.acceptComment(id);
      }
      if (kind === "reject") {
        return api.rejectComment(id);
      }
      if (kind === "reopen") {
        return api.reopenComment(id);
      }
      return api.resolveComment(id);
    },
    onSettled: () => {
      actionGuard.release();
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
    onError: (_error, _variables, context) => {
      queryClient.setQueryData(commentsQueryKey, context?.previous);
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
    actionErrorId: action.isError ? action.variables?.id : undefined,
    answeredAsksPending: answeredAsks.pending,
    asksPending: asks.isPending,
    commentsError: comments.isError,
    commentsPending: comments.isPending,
    commentRecords: comments.data ?? [],
    editComment: (id: string, body: string) => edit.mutateAsync({ body, id }),
    items,
    marginItems,
    needsYou,
    mutateItem: (input: { id: string; kind: MarginItemAction }) =>
      actionGuard.guard(() => action.mutate(input)),
    openAskCount,
    pendingActionId: action.isPending ? action.variables?.id : undefined,
    pinned: pinned.data ?? [],
    pinnedIds,
    resolvedThreads,
    isClosed: owner?.kind === "document" ? false : undefined,
    retryAnsweredAsk,
    retryComments: () => void comments.refetch(),
    retryItem: () => actionGuard.retryLast(action),
    threads,
  };
}

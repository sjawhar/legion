import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";

import { api } from "../../api/client";
import type { Artifact, Ask, Comment, Event } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { pinnedEventIds } from "../issue/pins";
import { useAnsweredAsks } from "./useAnsweredAsks";

export type MarginTab = "comments" | "pinned";
export type MarginItemAction = "accept" | "reject" | "resolve";
export type MarginItem =
  | { ask: Ask; depth: number; kind: "ask" }
  | { comment: Comment; depth: number; kind: "comment"; threadRootId: string };

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

function commentThreads(
  comments: Comment[]
): Array<Array<Extract<MarginItem, { kind: "comment" }>>> {
  const byParent = new Map<string, Comment[]>();
  const roots: Comment[] = [];
  const known = new Set(comments.map((comment) => comment.id));
  for (const comment of comments) {
    if (comment.reply_to === null || !known.has(comment.reply_to)) {
      roots.push(comment);
      continue;
    }
    const replies = byParent.get(comment.reply_to) ?? [];
    replies.push(comment);
    byParent.set(comment.reply_to, replies);
  }

  return [...roots]
    .sort((left, right) => right.created_at.localeCompare(left.created_at))
    .map((root) => {
      const thread: Array<{ comment: Comment; depth: number }> = [];
      const append = (comment: Comment, depth: number) => {
        thread.push({ comment, depth });
        for (const reply of [...(byParent.get(comment.id) ?? [])].sort((left, right) =>
          right.created_at.localeCompare(left.created_at)
        )) {
          append(reply, depth + 1);
        }
      };
      append(root, 0);
      return thread.map(({ comment, depth }) => ({
        comment,
        depth,
        kind: "comment" as const,
        threadRootId: root.id,
      }));
    });
}

export function marginItemId(item: MarginItem): string {
  return item.kind === "ask" ? item.ask.id : item.comment.id;
}

export function marginItemMarkId(item: MarginItem): string | undefined {
  return item.kind === "ask" ? item.ask.anchor?.mark_id : item.comment.anchor?.mark_id;
}

export function useMarginItems(
  issueKey: string | undefined,
  tab: MarginTab,
  visibleArtifact: Artifact | undefined,
  markPositions: ReadonlyMap<string, number>
) {
  const queryClient = useQueryClient();
  const asks = useQuery({ queryKey: ["inbox"], queryFn: () => api.getInbox() });
  const inboxOpenAsks = useMemo(
    () => (asks.data ?? []).filter((ask) => ask.issue_key === issueKey && ask.state === "open"),
    [asks.data, issueKey]
  );
  const comments = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["comments", issueKey],
    queryFn: () => api.listComments(issueKey ?? ""),
  });
  const userState = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["user-state"],
    queryFn: () => api.getMyState(),
  });
  const pinnedIds = pinnedEventIds(userState.data?.[issueKey ?? ""]?.dismissed ?? []);
  const pinned = useQuery({
    enabled: issueKey !== undefined && tab === "pinned" && pinnedIds.length > 0,
    queryKey: ["events", issueKey, "margin-pinned", pinnedIds],
    queryFn: () => fetchPinnedEvents(api.getIssueEvents.bind(api), issueKey ?? "", pinnedIds),
  });
  const answeredAsks = useAnsweredAsks(issueKey, visibleArtifact?.id);
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
  const items = useMemo<MarginItem[]>(() => {
    const anchoredItems = anchoredAsks.map((ask) => ({ ask, depth: 0, kind: "ask" as const }));
    const visibleComments = withoutAskThreadReplies(
      anchoredThreadComments(comments.data ?? [], visibleArtifact?.id)
    );
    const roots: MarginItem[][] = [
      ...anchoredItems.map((ask) => [ask]),
      ...commentThreads(visibleComments),
    ];
    return roots
      .sort((left, right) => {
        const leftRoot = left[0];
        const rightRoot = right[0];
        if (leftRoot === undefined || rightRoot === undefined) {
          return 0;
        }
        const leftMarkId = marginItemMarkId(leftRoot);
        const rightMarkId = marginItemMarkId(rightRoot);
        const leftPosition = leftMarkId === undefined ? undefined : markPositions.get(leftMarkId);
        const rightPosition =
          rightMarkId === undefined ? undefined : markPositions.get(rightMarkId);
        if (leftPosition !== undefined && rightPosition !== undefined) {
          if (leftPosition !== rightPosition) {
            return leftPosition - rightPosition;
          }
        } else if (leftPosition !== undefined || rightPosition !== undefined) {
          return leftPosition === undefined ? 1 : -1;
        }
        const leftAnchor = leftRoot.kind === "ask" ? leftRoot.ask.anchor : leftRoot.comment.anchor;
        const rightAnchor =
          rightRoot.kind === "ask" ? rightRoot.ask.anchor : rightRoot.comment.anchor;
        const leftCreatedAt =
          leftRoot.kind === "ask" ? leftRoot.ask.created_at : leftRoot.comment.created_at;
        const rightCreatedAt =
          rightRoot.kind === "ask" ? rightRoot.ask.created_at : rightRoot.comment.created_at;
        // Every anchored ask is always anchored (unanchored asks never enter this list, see
        // useAnsweredAsks), while the comment filter admits only document-anchored threads.
        // This puts document review in reading order; an item whose text has changed sorts
        // after found anchors.
        if (leftAnchor !== null && rightAnchor !== null) {
          return leftCreatedAt.localeCompare(rightCreatedAt);
        }
        if (leftAnchor !== null || rightAnchor !== null) {
          return leftAnchor !== null ? -1 : 1;
        }
        return rightCreatedAt.localeCompare(leftCreatedAt);
      })
      .flat();
  }, [anchoredAsks, comments.data, markPositions, visibleArtifact]);
  const marginItems = useMemo<MarginItem[]>(
    () => [...needsYou.map((ask) => ({ ask, depth: 0, kind: "ask" as const })), ...items],
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
      return api.resolveComment(id);
    },
    onSettled: () => {
      actionGuard.release();
    },
    onMutate: async ({ id, kind }) => {
      const commentsKey = ["comments", issueKey];
      await queryClient.cancelQueries({ queryKey: commentsKey });
      const previous = queryClient.getQueryData<Comment[]>(commentsKey);
      queryClient.setQueryData<Comment[]>(commentsKey, (current) =>
        current?.map((comment) => {
          if (comment.id !== id) {
            return comment;
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
      queryClient.setQueryData(["comments", issueKey], context?.previous);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["issue", issueKey] });
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
    items,
    marginItems,
    needsYou,
    mutateItem: (input: { id: string; kind: MarginItemAction }) =>
      actionGuard.guard(() => action.mutate(input)),
    pendingActionId: action.isPending ? action.variables?.id : undefined,
    openAskCount,
    pinned: pinned.data ?? [],
    pinnedIds,
    retryAnsweredAsk,
    retryComments: () => void comments.refetch(),
    retryItem: () => actionGuard.retryLast(action),
  };
}

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import { useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { Ask, Comment } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";
import { pinnedEventIds } from "../issue/log-model";
import { parseIssuePath } from "../refs/routes";
import { useAnsweredAsks } from "./useAnsweredAsks";

export type MarginTab = "artifacts" | "comments" | "pinned";
export type MarginItemAction = "accept" | "reject" | "resolve";
export type MarginItem =
  | { ask: Ask; depth: number; kind: "ask" }
  | { comment: Comment; depth: number; kind: "comment" };

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

function commentThreads(comments: Comment[]): Array<Array<{ comment: Comment; depth: number }>> {
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
      return thread;
    });
}

export function marginItemId(item: MarginItem): string {
  return item.kind === "ask" ? item.ask.id : item.comment.id;
}

export function useMarginItems(tab: MarginTab) {
  const { pathname } = useLocation();
  const queryClient = useQueryClient();
  const route = parseIssuePath(pathname);
  const issueKey = route?.key;
  const routeArtifactSlug = route?.kind === "artifact" ? route.slug : undefined;
  const routeItemId = route?.kind === "ask" || route?.kind === "comment" ? route.id : undefined;
  const issue = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["issue", issueKey],
    queryFn: () => api.getIssue(issueKey ?? ""),
  });
  const visibleArtifact =
    routeArtifactSlug === undefined
      ? issue.data?.artifacts.find((artifact) => artifact.id === issue.data?.primary_artifact_id)
      : issue.data?.artifacts.find((artifact) => artifact.slug === routeArtifactSlug);
  const asks = useQuery({ queryKey: ["inbox"], queryFn: () => api.getInbox() });
  const openAskCount = (asks.data ?? []).filter(
    (ask) => ask.issue_key === issueKey && ask.state === "open"
  ).length;
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
    queryFn: () => api.getIssueEvents(issueKey ?? "", { ids: pinnedIds }),
  });
  const answeredAsks = useAnsweredAsks(issueKey, visibleArtifact?.id);
  const anchoredAsks = answeredAsks.asks;
  const items = useMemo<MarginItem[]>(() => {
    const anchoredItems = anchoredAsks.map((ask) => ({ ask, depth: 0, kind: "ask" as const }));
    const visibleComments = withoutAskThreadReplies(
      (comments.data ?? []).filter(
        (comment) => comment.anchor === null || comment.anchor.artifact_id === visibleArtifact?.id
      )
    );
    const threads = commentThreads(visibleComments).map((thread) =>
      thread.map(({ comment, depth }) => ({ comment, depth, kind: "comment" as const }))
    );
    const roots: MarginItem[][] = [...anchoredItems.map((ask) => [ask]), ...threads];
    return roots
      .sort((left, right) => {
        const leftRoot = left[0];
        const rightRoot = right[0];
        if (leftRoot === undefined || rightRoot === undefined) {
          return 0;
        }
        const leftAnchor = leftRoot.kind === "ask" ? leftRoot.ask.anchor : leftRoot.comment.anchor;
        const rightAnchor =
          rightRoot.kind === "ask" ? rightRoot.ask.anchor : rightRoot.comment.anchor;
        // Every anchored ask is always anchored (unanchored asks never enter this list, see
        // useAnsweredAsks), so this puts anchored asks and anchored comments in document
        // reading order; a general, unanchored comment sorts after any anchored item, most
        // recent first among its own kind.
        if (leftAnchor !== null && rightAnchor !== null) {
          return leftAnchor.from - rightAnchor.from || leftAnchor.to - rightAnchor.to;
        }
        if (leftAnchor !== null || rightAnchor !== null) {
          return leftAnchor !== null ? -1 : 1;
        }
        const leftCreatedAt =
          leftRoot.kind === "ask" ? leftRoot.ask.created_at : leftRoot.comment.created_at;
        const rightCreatedAt =
          rightRoot.kind === "ask" ? rightRoot.ask.created_at : rightRoot.comment.created_at;
        return rightCreatedAt.localeCompare(leftCreatedAt);
      })
      .flat();
  }, [anchoredAsks, comments.data, visibleArtifact]);
  const decorationAnchors = useMemo(
    () =>
      items.flatMap((item) => {
        const anchor = item.kind === "ask" ? item.ask.anchor : item.comment.anchor;
        const open = item.kind === "ask" ? item.ask.state === "open" : !item.comment.resolved;
        return anchor === null || anchor.orphaned || !open
          ? []
          : [{ anchor, id: marginItemId(item) }];
      }),
    [items]
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
    decorationAnchors,
    isClosed: issue.data !== undefined && issue.data.closed_at !== null,
    issueError: issue.isError,
    issueKey,
    issuePending: issue.isPending,
    items,
    mutateItem: (input: { id: string; kind: MarginItemAction }) =>
      actionGuard.guard(() => action.mutate(input)),
    pendingActionId: action.isPending ? action.variables?.id : undefined,
    openAskCount,
    pinned: pinned.data ?? [],
    pinnedIds,
    retryAnsweredAsk,
    retryComments: () => void comments.refetch(),
    retryIssue: () => void issue.refetch(),
    retryItem: () => actionGuard.retryLast(action),
    routeArtifactSlug,
    routeItemId,
    visibleArtifact,
  };
}

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";
import { useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { Ask, Comment } from "../../api/types";
import { pinnedEventIds } from "../issue/log-model";
import { parseIssuePath } from "../refs/routes";

export type MarginTab = "artifacts" | "comments" | "pinned";
export type MarginItemAction = "accept" | "reject" | "resolve";
export type MarginItem =
  | { ask: Ask; depth: number; kind: "ask" }
  | { comment: Comment; depth: number; kind: "comment" };

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
    enabled: issueKey !== undefined && visibleArtifact !== undefined,
    queryKey: ["comments", issueKey, visibleArtifact?.id],
    queryFn: () => api.listComments(issueKey ?? "", visibleArtifact?.id),
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
  const items = useMemo<MarginItem[]>(() => {
    if (visibleArtifact === undefined) {
      return [];
    }
    const anchoredAsks = (asks.data ?? [])
      .filter((ask) => ask.issue_key === issueKey && ask.anchor?.artifact_id === visibleArtifact.id)
      .map((ask) => ({ ask, depth: 0, kind: "ask" as const }));
    const threads = commentThreads(comments.data ?? []).map((thread) =>
      thread.map(({ comment, depth }) => ({ comment, depth, kind: "comment" as const }))
    );
    const roots: MarginItem[][] = [...anchoredAsks.map((ask) => [ask]), ...threads];
    return roots
      .sort((left, right) => {
        const leftRoot = left[0];
        const rightRoot = right[0];
        if (leftRoot === undefined || rightRoot === undefined) {
          return 0;
        }
        const leftCreatedAt =
          leftRoot.kind === "ask" ? leftRoot.ask.created_at : leftRoot.comment.created_at;
        const rightCreatedAt =
          rightRoot.kind === "ask" ? rightRoot.ask.created_at : rightRoot.comment.created_at;
        return rightCreatedAt.localeCompare(leftCreatedAt);
      })
      .flat();
  }, [asks.data, comments.data, issueKey, visibleArtifact]);
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
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
      void queryClient.invalidateQueries({ queryKey: ["issue", issueKey] });
    },
  });

  return {
    asksPending: asks.isPending,
    commentsPending: comments.isPending,
    decorationAnchors,
    isClosed: issue.data !== undefined && issue.data.closed_at !== null,
    issueKey,
    issuePending: issue.isPending,
    items,
    mutateItem: action.mutate,
    openAskCount,
    pinned: pinned.data ?? [],
    pinnedIds,
    routeArtifactSlug,
    routeItemId,
    visibleArtifact,
  };
}

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  type ReactNode,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useLocation } from "react-router-dom";

import { api } from "../../api/client";
import type { Anchor, Ask, Comment, Event } from "../../api/types";
import { AskCard } from "../inbox/AskCard";
import { pinnedEventIds } from "../issue/log-model";
import { Unfurl } from "../refs/Unfurl";
import { Composer, type ComposerAnchor, type ComposerKind } from "./Composer";
import { SelectionMenu } from "./SelectionMenu";

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

type MarginTab = "artifacts" | "comments" | "pinned";
type MarginItem =
  | { ask: Ask; depth: number; kind: "ask" }
  | { comment: Comment; depth: number; kind: "comment" };

interface ComposerState {
  anchor: ComposerAnchor;
  kind: ComposerKind;
  replyTo?: string;
}

interface MarginProps {
  ArtifactsTabSlot?: () => ReactNode;
}

function issueKeyFromPath(pathname: string): string | undefined {
  return pathname.match(/^\/issues\/([^/]+)/)?.[1];
}

function artifactSlugFromPath(pathname: string): string | undefined {
  return pathname.match(/^\/issues\/[^/]+\/artifact\/([^/]+)/)?.[1];
}

function newestFirst<T extends { created_at: string }>(items: T[]): T[] {
  return [...items].sort((left, right) => right.created_at.localeCompare(left.created_at));
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

  return newestFirst(roots).map((root) => {
    const thread: Array<{ comment: Comment; depth: number }> = [];
    const append = (comment: Comment, depth: number) => {
      thread.push({ comment, depth });
      for (const reply of newestFirst(byParent.get(comment.id) ?? [])) {
        append(reply, depth + 1);
      }
    };
    append(root, 0);
    return thread;
  });
}

function itemId(item: MarginItem): string {
  return item.kind === "ask" ? item.ask.id : item.comment.id;
}

function itemAnchor(item: MarginItem): Anchor | null {
  return item.kind === "ask" ? item.ask.anchor : item.comment.anchor;
}

function itemCreatedAt(item: MarginItem): string {
  return item.kind === "ask" ? item.ask.created_at : item.comment.created_at;
}

function isOpen(item: MarginItem): boolean {
  return item.kind === "ask" ? item.ask.state === "open" : !item.comment.resolved;
}

function eventLabel(event: Event): string {
  return event.type.replace(".", " ");
}

function CommentCard({
  comment,
  depth,
  onAction,
  onReply,
  selected,
}: {
  comment: Comment;
  depth: number;
  onAction: (id: string, action: "accept" | "reject" | "resolve") => void;
  onReply: (comment: Comment) => void;
  selected: boolean;
}): ReactNode {
  const suggestion = comment.suggestion;
  const anchor = comment.anchor;
  return (
    <article
      className={`rounded-xl border p-3 text-sm shadow-sm ${
        selected ? "border-sky-500 bg-sky-50" : "border-slate-200 bg-white"
      }`}
      data-margin-item={comment.id}
      data-testid={`margin-comment-${comment.id}`}
      style={{ marginLeft: `${depth * 12}px` }}
    >
      {anchor === null ? null : (
        <blockquote className="mb-2 border-l-2 border-sky-400 pl-2 text-slate-600">
          {anchor.quote}
        </blockquote>
      )}
      {anchor?.orphaned ? (
        <p className="mb-2 text-xs font-medium text-amber-800">
          Text changed.{" "}
          <Link
            className="underline"
            to={`/issues/${comment.issue_key}/artifacts/${anchor.artifact_id}/versions/${anchor.version}?from=${anchor.from}&to=${anchor.to}`}
          >
            View original text
          </Link>
        </p>
      ) : null}
      {suggestion === null ? <p className="whitespace-pre-wrap">{comment.body}</p> : null}
      {suggestion !== null && anchor !== null ? (
        <div className="space-y-1 font-mono text-xs">
          <del className="block rounded bg-rose-50 px-2 py-1 text-rose-800">{anchor.quote}</del>
          <ins className="block rounded bg-emerald-50 px-2 py-1 text-emerald-800">
            {suggestion.replace_with}
          </ins>
          {comment.body === "Suggested replacement." ? null : (
            <p className="whitespace-pre-wrap font-sans text-slate-700">{comment.body}</p>
          )}
        </div>
      ) : null}
      <Unfurl body={comment.body} />
      <p className="mt-2 text-xs text-slate-500">
        {comment.author.id} · {new Date(comment.created_at).toLocaleString()}
      </p>
      <div className="mt-2 flex flex-wrap gap-3 text-sm">
        {suggestion !== null && suggestion.accepted === null && !comment.resolved ? (
          <>
            <button
              className="font-medium text-emerald-700 hover:text-emerald-900"
              onClick={() => onAction(comment.id, "accept")}
              type="button"
            >
              Accept
            </button>
            <button
              className="font-medium text-rose-700 hover:text-rose-900"
              onClick={() => onAction(comment.id, "reject")}
              type="button"
            >
              Reject
            </button>
          </>
        ) : suggestion === null && !comment.resolved ? (
          <button
            className="font-medium text-sky-700 hover:text-sky-900"
            onClick={() => onAction(comment.id, "resolve")}
            type="button"
          >
            Resolve
          </button>
        ) : (
          <span className="font-medium text-slate-500">
            {suggestion?.accepted === true
              ? "Accepted"
              : suggestion?.accepted === false
                ? "Rejected"
                : "Resolved"}
          </span>
        )}
        {anchor === null ? null : (
          <button
            className="font-medium text-sky-700 hover:text-sky-900"
            onClick={() => onReply(comment)}
            type="button"
          >
            Reply
          </button>
        )}
      </div>
    </article>
  );
}

export function Margin({ ArtifactsTabSlot }: MarginProps): ReactNode {
  const { pathname } = useLocation();
  const queryClient = useQueryClient();
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
  const list = useRef<HTMLDivElement>(null);
  const scrolledRouteItem = useRef<string | undefined>(undefined);
  const [composer, setComposer] = useState<ComposerState>();
  const [expandedIssueKey, setExpandedIssueKey] = useState<string>();
  const sheetDragOrigin = useRef<number | undefined>(undefined);
  const sheetDragMoved = useRef(false);
  const issueKey = issueKeyFromPath(pathname);
  const routeArtifactSlug = artifactSlugFromPath(pathname);
  const routeItemId = pathname.match(/^\/issues\/[^/]+\/(?:asks|comments)\/([^/]+)$/)?.[1];
  const issue = useQuery({
    enabled: issueKey !== undefined,
    queryKey: ["issue", issueKey],
    queryFn: () => api.getIssue(issueKey ?? ""),
  });
  const visibleArtifact =
    routeArtifactSlug === undefined
      ? issue.data?.artifacts?.find((artifact) => artifact.id === issue.data?.primary_artifact_id)
      : issue.data?.artifacts?.find((artifact) => artifact.slug === routeArtifactSlug);
  const asks = useQuery({ queryKey: ["inbox"], queryFn: () => api.getInbox() });
  const openAskCount = (asks.data ?? []).filter(
    (ask) => ask.issue_key === issueKey && ask.state === "open"
  ).length;
  const sheetExpanded = issueKey !== undefined && expandedIssueKey === issueKey;
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
        return itemCreatedAt(rightRoot).localeCompare(itemCreatedAt(leftRoot));
      })
      .flat();
  }, [asks.data, comments.data, issueKey, visibleArtifact]);
  const decorationAnchors = useMemo(
    () =>
      items.flatMap((item) => {
        const anchor = itemAnchor(item);
        return anchor === null || anchor.orphaned || !isOpen(item)
          ? []
          : [{ anchor, id: itemId(item) }];
      }),
    [items]
  );
  const action = useMutation({
    mutationFn: ({ id, kind }: { id: string; kind: "accept" | "reject" | "resolve" }) => {
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

  useEffect(() => {
    setAnchors(decorationAnchors);
  }, [decorationAnchors, setAnchors]);
  useEffect(() => {
    if (routeItemId !== undefined) {
      setTab("comments");
      selectItem(routeItemId);
    }
  }, [routeItemId, selectItem]);
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

  useEffect(() => {
    if (routeItemId === undefined) {
      scrolledRouteItem.current = undefined;
      return;
    }
    if (
      tab !== "comments" ||
      scrolledRouteItem.current === routeItemId ||
      !items.some((item) => itemId(item) === routeItemId) ||
      (window.matchMedia("(max-width: 767px)").matches && !sheetExpanded)
    ) {
      return;
    }
    const container = list.current;
    if (container === null) {
      return;
    }
    const card = container.querySelector<HTMLElement>(
      `[data-margin-item="${CSS.escape(routeItemId)}"]`
    );
    if (card === null) {
      return;
    }
    const top =
      card.getBoundingClientRect().top -
      container.getBoundingClientRect().top +
      container.scrollTop -
      container.clientHeight / 4;
    container.scrollTo({ top: Math.max(0, top) });
    scrolledRouteItem.current = routeItemId;
  }, [items, routeItemId, sheetExpanded, tab]);

  useEffect(() => {
    const container = list.current;
    if (container === null) {
      return;
    }
    const cardForTarget = (target: EventTarget | null) =>
      target instanceof Element ? target.closest<HTMLElement>("[data-margin-item]") : null;
    const selectCard = (event: MouseEvent) => {
      const card = cardForTarget(event.target);
      if (card?.dataset.marginItem !== undefined) {
        selectItem(card.dataset.marginItem);
      }
    };
    const hoverCard = (event: MouseEvent) => {
      const card = cardForTarget(event.target);
      if (card?.dataset.marginItem !== undefined) {
        setHoveredItemId(card.dataset.marginItem);
      }
    };
    const leaveCard = (event: MouseEvent) => {
      if (cardForTarget(event.target) !== cardForTarget(event.relatedTarget)) {
        setHoveredItemId(undefined);
      }
    };
    container.addEventListener("click", selectCard);
    container.addEventListener("mouseover", hoverCard);
    container.addEventListener("mouseout", leaveCard);
    return () => {
      container.removeEventListener("click", selectCard);
      container.removeEventListener("mouseover", hoverCard);
      container.removeEventListener("mouseout", leaveCard);
    };
  }, [selectItem, setHoveredItemId]);

  useEffect(() => {
    if (selection !== undefined && selection.artifactId !== visibleArtifact?.id) {
      setSelection(undefined);
    }
  }, [selection, setSelection, visibleArtifact?.id]);

  const openComposer = (kind: ComposerKind, anchor: ComposerAnchor, replyTo?: string) => {
    setComposer({ anchor, kind, replyTo });
    setSelection(undefined);
  };
  const closeComposer = () => setComposer(undefined);
  const isClosed = issue.data?.closed_at !== null && issue.data !== undefined;

  return (
    <aside
      className={`fixed inset-x-0 bottom-0 z-10 border-t border-slate-200 bg-white shadow-[0_-8px_24px_rgba(15,23,42,0.08)] ${
        sheetExpanded ? "max-h-[85dvh] overflow-y-auto p-4" : "h-16 overflow-hidden"
      } md:static md:order-3 md:h-auto md:max-h-none md:w-96 md:overflow-visible md:border-t-0 md:border-l md:p-4 md:shadow-none`}
      data-expanded={sheetExpanded ? "true" : "false"}
      data-testid="margin-sheet"
    >
      <button
        aria-expanded={sheetExpanded}
        aria-label="Open review panel"
        className="flex h-16 w-full items-center justify-between px-4 text-left text-sm font-semibold text-slate-800 md:hidden"
        onClick={() => {
          if (sheetDragMoved.current) {
            sheetDragMoved.current = false;
            return;
          }
          setExpandedIssueKey(sheetExpanded ? undefined : issueKey);
        }}
        onPointerDown={(event) => {
          sheetDragOrigin.current = event.clientY;
        }}
        onPointerUp={(event) => {
          const origin = sheetDragOrigin.current;
          sheetDragOrigin.current = undefined;
          if (origin === undefined || Math.abs(event.clientY - origin) < 12) {
            return;
          }
          sheetDragMoved.current = true;
          setExpandedIssueKey(event.clientY < origin ? issueKey : undefined);
        }}
        type="button"
      >
        <span>Review panel</span>
        <span className="font-normal text-slate-500">
          {openAskCount} open {openAskCount === 1 ? "ask" : "asks"}
        </span>
      </button>
      <div className={sheetExpanded ? "px-4 pb-4 md:px-0 md:pb-0" : "hidden md:block"}>
        {selection === undefined || visibleArtifact === undefined || isClosed ? null : (
          <SelectionMenu onAction={(kind) => openComposer(kind, selection)} selection={selection} />
        )}
        <div className="flex border-b border-slate-200" role="tablist">
          {(["comments", "artifacts", "pinned"] as MarginTab[]).map((name) => (
            <button
              aria-selected={tab === name}
              className={
                tab === name
                  ? "border-b-2 border-sky-600 px-3 py-2 text-sm font-semibold text-sky-700"
                  : "px-3 py-2 text-sm text-slate-600"
              }
              key={name}
              onClick={() => setTab(name)}
              role="tab"
              type="button"
            >
              {name === "comments" ? "Comments" : name === "artifacts" ? "Artifacts" : "Pinned"}
            </button>
          ))}
        </div>
        {issueKey === undefined ? (
          <p className="pt-3 text-sm text-slate-500">Open an issue to review its margin.</p>
        ) : null}
        {issueKey !== undefined && visibleArtifact === undefined && issue.isPending ? (
          <p className="pt-3 text-sm text-slate-500">Loading margin…</p>
        ) : null}
        {tab === "artifacts" ? ArtifactsTabSlot === undefined ? null : <ArtifactsTabSlot /> : null}
        {tab === "pinned" ? (
          <div className="max-h-[45dvh] overflow-y-auto pt-3">
            {(pinned.data ?? []).map((event) => (
              <article className="rounded-lg border border-slate-200 p-3 text-sm" key={event.id}>
                <p className="font-medium text-slate-900">{eventLabel(event)}</p>
                <p className="mt-1 text-xs text-slate-500">{event.actor.id}</p>
              </article>
            ))}
            {pinnedIds.length === 0 ? (
              <p className="text-sm text-slate-500">No pinned items.</p>
            ) : null}
          </div>
        ) : null}
        {tab === "comments" && visibleArtifact !== undefined ? (
          <div className="space-y-3 pt-3">
            {composer === undefined ? null : (
              <Composer
                anchor={composer.anchor}
                kind={composer.kind}
                issueKey={issueKey ?? ""}
                onClose={closeComposer}
                replyTo={composer.replyTo}
              />
            )}
            <section
              aria-label="Margin review items"
              className="max-h-[45dvh] space-y-3 overflow-y-auto"
              ref={list}
            >
              {items.map((item) => {
                const id = itemId(item);
                const active = selectedItemId === id || hoveredItemId === id;
                return (
                  <div key={id}>
                    {item.kind === "ask" ? (
                      <div
                        className={active ? "rounded-xl ring-2 ring-sky-400" : undefined}
                        data-margin-item={id}
                      >
                        <AskCard ask={item.ask} />
                        <Unfurl body={item.ask.question} />
                        <p className="mt-2 text-xs text-slate-500">
                          {new Date(item.ask.created_at).toLocaleString()}
                        </p>
                      </div>
                    ) : (
                      <CommentCard
                        comment={item.comment}
                        depth={item.depth}
                        onAction={(commentID, kind) => action.mutate({ id: commentID, kind })}
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
                        selected={active}
                      />
                    )}
                  </div>
                );
              })}
              {items.length === 0 && !asks.isPending && !comments.isPending ? (
                <p className="text-sm text-slate-500">
                  No comments, asks, or suggestions on this document.
                </p>
              ) : null}
            </section>
          </div>
        ) : null}
      </div>
    </aside>
  );
}

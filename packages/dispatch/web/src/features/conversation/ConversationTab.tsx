import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Agent, Event, UserIssueState, UserState } from "../../api/types";
import { PinButton } from "../../components/PinButton";
import {
  checkboxAccent,
  dangerText,
  linkText,
  newDividerLine,
  primaryButtonBg,
  primaryButtonHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedHoverBg,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { AskCard } from "../inbox/AskCard";
import { EventBody } from "../issue/EventBody";
import { eventItemId, isPinnedEvent } from "../issue/pins";
import {
  applyPinStateOperation,
  IssueStateWriteQueue,
  type PinStateOperation,
} from "../issue/state-write-queue";
import { CopyRefButton } from "../refs/CopyRefButton";
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { ViewportAnchor } from "../shell/ViewportAnchor";
import { Avatar } from "./Avatar";
import { type Author, resolveAuthor } from "./authors";
import { ConversationComposer, type ReplyTarget } from "./ConversationComposer";
import {
  buildConversationItems,
  type ConversationItem,
  countRetractedAsks,
  dateKey,
  type MessageDeliveryEvent,
  type MessageEvent,
  type ThreadReply,
  visibleConversationItems,
} from "./conversation-model";
import { ReplyButton } from "./ReplyButton";
import { firstLine, ReplyQuote, replyQuoteText } from "./ReplyQuote";
import { ReplyTurn, ThreadReplies, TurnActions } from "./ReplyTurn";
import { TargetedMessageCard } from "./TargetedMessageCard";
import { useFollowLatest } from "./use-follow-latest";
import { useShowActivity, useShowRetracted } from "./use-show-activity";
import { useAgents } from "./useAgents";

const stateWrites = new IssueStateWriteQueue();

interface FailedStateOperations {
  authoritativeState: UserIssueState | undefined;
  issueKey: string;
  operations: PinStateOperation[];
}

interface ConversationTabProps {
  focusItemId?: string;
  isClosed: boolean;
  issueKey: string;
  route?: string | null;
  state: UserState | undefined;
  visible: boolean;
}

function eventItems(data: { pages: Event[][] } | undefined): Event[] {
  return data?.pages.flat() ?? [];
}

function eventState(state: UserState | undefined, issueKey: string): UserIssueState {
  return state?.[issueKey] ?? { dismissed: [], last_read_seq: 0, pinned: false };
}

const maxFocusPageLoads = 10;

type ThreadRoot = Extract<ConversationItem, { kind: "message" | "targeted-message" }>;

/** The delivery a reply into this thread inherits: the root's target, in the mode of the
 *  thread's most recent attempt (root or any reply), named after the session that received it. */
function threadDelivery(
  root: ThreadRoot,
  agents: readonly Agent[]
): ReplyTarget["thread"] | undefined {
  if (root.kind !== "targeted-message" || root.event.payload.target === null) {
    return undefined;
  }
  let last: MessageDeliveryEvent | undefined;
  for (const attempt of [
    ...root.deliveries,
    ...root.replies.flatMap((reply) => reply.deliveries),
  ]) {
    if (last === undefined || attempt.seq > last.seq) last = attempt;
  }
  const target = root.event.payload.target;
  const session = agents.find((agent) => agent.session_id === last?.payload.session_id);
  return {
    delivery: last?.payload.delivery ?? "steer",
    target,
    title: last?.payload.title || session?.title || target,
  };
}

/** What the composer quotes when the reader replies to `node`. */
function replyTargetFor(
  node: { event: MessageEvent },
  author: Author,
  root: ThreadRoot,
  issueKey: string,
  agents: readonly Agent[]
): ReplyTarget {
  const thread = threadDelivery(root, agents);
  return {
    author: author.label,
    excerpt: firstLine(node.event.payload.body),
    id: node.event.payload.id,
    to: buildIssuePath({ id: node.event.payload.id, key: issueKey, kind: "message" }),
    ...(thread === undefined ? {} : { thread }),
  };
}

/** A reply in a thread as the Conversation renders it: the quoted parent links to that turn,
 *  a delivered reply (a human's follow-up on a targeted thread) shows its attempt and keeps the
 *  retry row while unanswered, and every reply can be replied to while the issue is open. */
function ConversationReply({
  agents,
  currentTurnId,
  isClosed,
  issueKey,
  onReply,
  reply,
  root,
  titles,
}: {
  agents: readonly Agent[];
  currentTurnId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  onReply: (target: ReplyTarget) => void;
  reply: ThreadReply;
  root: ThreadRoot;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: "btw" | "steer") =>
      api.createMessageDelivery(reply.event.payload.id, delivery),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["events", issueKey] }),
  });
  const author = resolveAuthor(reply.author, titles);
  const parentId = reply.event.payload.in_reply_to;
  const lastAttempt = reply.deliveries.at(-1);
  const target = agents.find((agent) => agent.session_id === lastAttempt?.payload.session_id);
  const targetName = lastAttempt?.payload.title || target?.title || "agent";
  const answer = root.replies.find(
    (candidate) =>
      candidate.event.payload.in_reply_to === reply.event.payload.id &&
      candidate.author.kind === "session"
  );
  const parent = [root, ...root.replies].find((node) => node.event.payload.id === parentId);
  return (
    <ReplyTurn
      actions={
        <CopyRefButton route={{ id: reply.event.payload.id, key: issueKey, kind: "message" }} />
      }
      at={reply.at}
      author={author}
      body={<EventBody event={reply.event} />}
      current={reply.id === currentTurnId}
      delivery={
        lastAttempt === undefined
          ? undefined
          : {
              answeredBy:
                answer === undefined ? undefined : resolveAuthor(answer.author, titles).label,
              attempts: reply.deliveries.map((attempt) => ({
                attempt: attempt.payload.attempt,
                createdAt: attempt.created_at,
                delivery: attempt.payload.delivery,
                error: attempt.payload.error,
                state: attempt.payload.state,
                targetName: attempt.payload.title,
              })),
              retry: isClosed
                ? undefined
                : {
                    canBtw: target?.capabilities.includes("btw") !== false,
                    onRetry: retry.mutate,
                    retrying: retry.isPending,
                  },
              targetName,
            }
      }
      onReply={
        isClosed ? undefined : () => onReply(replyTargetFor(reply, author, root, issueKey, agents))
      }
      quote={{
        text: replyQuoteText(
          parent === undefined ? undefined : resolveAuthor(parent.author, titles).label,
          parent === undefined
            ? (reply.event.payload.reply_body ?? "")
            : firstLine(parent.event.payload.body)
        ),
        to:
          parentId === null || parentId === undefined
            ? undefined
            : buildIssuePath({ id: parentId, key: issueKey, kind: "message" }),
      }}
      turnID={reply.id}
    />
  );
}

function Thread({
  agents,
  currentTurnId,
  isClosed,
  issueKey,
  onReply,
  root,
  titles,
}: {
  agents: readonly Agent[];
  currentTurnId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  onReply: (target: ReplyTarget) => void;
  root: ThreadRoot;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  if (root.replies.length === 0) return null;
  return (
    <ThreadReplies>
      {root.replies.map((reply) => (
        <ConversationReply
          agents={agents}
          currentTurnId={currentTurnId}
          isClosed={isClosed}
          issueKey={issueKey}
          key={reply.id}
          onReply={onReply}
          reply={reply}
          root={root}
          titles={titles}
        />
      ))}
    </ThreadReplies>
  );
}

function TurnPin({
  disabled,
  onPin,
  pinned,
}: {
  disabled: boolean;
  onPin: () => void;
  pinned: boolean;
}): ReactNode {
  return (
    <PinButton
      disabled={disabled}
      label={pinned ? "Unpin" : "Pin"}
      onClick={onPin}
      pinned={pinned}
      quiet
    />
  );
}

function MessageTurn({
  agents,
  author,
  current,
  currentTurnId,
  disabled,
  isClosed,
  issueKey,
  item,
  onPin,
  onReply,
  pinned,
  register,
  titles,
}: {
  agents: readonly Agent[];
  author: Author;
  current: boolean;
  currentTurnId: string | undefined;
  disabled: boolean;
  isClosed: boolean;
  issueKey: string;
  item: Extract<ConversationItem, { kind: "message" | "comment" }>;
  onPin: () => void;
  onReply: (target: ReplyTarget) => void;
  pinned: boolean;
  register: (element: HTMLElement | null) => void;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const replyTo = item.kind === "message" ? item.event.payload.in_reply_to : null;
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`group rounded-lg px-2 ${item.continued ? "py-0.5" : "mt-2 py-1"} ${surfaceMutedHoverBg}`}
      data-continued={String(item.continued)}
      data-event-seq={item.lastSeq}
      data-turn={item.id}
      ref={register}
    >
      <div className="flex gap-3">
        {item.continued ? <span className="w-8 shrink-0" /> : <Avatar author={author} />}
        <div className="min-w-0 flex-1">
          {item.continued ? null : (
            <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnSurface}`}>
              <span className="font-semibold">{author.label}</span>
              <Timestamp at={item.at} />
            </p>
          )}
          {replyTo === null || replyTo === undefined ? null : (
            <ReplyQuote
              className="mb-1"
              to={buildIssuePath({ id: replyTo, key: issueKey, kind: "message" })}
            >
              {replyQuoteText(
                undefined,
                item.kind === "message" ? (item.event.payload.reply_body ?? "") : ""
              )}
            </ReplyQuote>
          )}
          <EventBody event={item.event} />
        </div>
        <TurnActions>
          <CopyRefButton route={{ id: item.event.payload.id, key: issueKey, kind: item.kind }} />
          {item.kind === "message" && !isClosed ? (
            <ReplyButton
              onClick={() => onReply(replyTargetFor(item, author, item, issueKey, agents))}
            />
          ) : null}
          <TurnPin disabled={disabled} onPin={onPin} pinned={pinned} />
        </TurnActions>
      </div>
      {item.kind === "message" ? (
        <Thread
          agents={agents}
          currentTurnId={currentTurnId}
          isClosed={isClosed}
          issueKey={issueKey}
          onReply={onReply}
          root={item}
          titles={titles}
        />
      ) : null}
    </li>
  );
}

function TargetedMessageTurn({
  agents,
  current,
  currentTurnId,
  isClosed,
  issueKey,
  item,
  onReply,
  register,
  titles,
}: {
  agents: readonly Agent[];
  current: boolean;
  currentTurnId: string | undefined;
  isClosed: boolean;
  issueKey: string;
  item: Extract<ConversationItem, { kind: "targeted-message" }>;
  onReply: (target: ReplyTarget) => void;
  register: (element: HTMLElement | null) => void;
  titles: ReadonlyMap<string, string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const retry = useMutation({
    mutationFn: (delivery: "btw" | "steer") =>
      api.createMessageDelivery(item.event.payload.id, delivery),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: ["events", item.event.issue_key] }),
  });
  const delivery = item.deliveries.at(-1);
  const target =
    delivery === undefined
      ? undefined
      : agents.find((agent) => agent.session_id === delivery.payload.session_id);
  const targetName =
    delivery?.payload.title || target?.title || item.event.payload.target || "agent";
  const asker = resolveAuthor(item.author, titles);

  return (
    <TargetedMessageCard
      answeredBy={
        item.answer === undefined ? undefined : resolveAuthor(item.answer.author, titles).label
      }
      body={
        <>
          <p className={`flex flex-wrap items-center gap-x-2 text-sm ${textSecondaryOnSurface}`}>
            <span className="font-semibold">{asker.label}</span>
            <Timestamp at={item.at} />
            <CopyRefButton route={{ id: item.event.payload.id, key: issueKey, kind: "message" }} />
          </p>
          <EventBody event={item.event} />
        </>
      }
      canBtw={target?.capabilities.includes("btw") !== false}
      current={current}
      deliveries={item.deliveries.map((attempt) => ({
        attempt: attempt.payload.attempt,
        createdAt: attempt.created_at,
        delivery: attempt.payload.delivery,
        error: attempt.payload.error,
        state: attempt.payload.state,
        targetName: attempt.payload.title,
      }))}
      header={<Avatar author={asker} />}
      isClosed={isClosed}
      lastSeq={item.lastSeq}
      onReply={() => onReply(replyTargetFor(item, asker, item, issueKey, agents))}
      onRetry={retry.mutate}
      register={register}
      retrying={retry.isPending}
      targetName={targetName}
      thread={
        <Thread
          agents={agents}
          currentTurnId={currentTurnId}
          isClosed={isClosed}
          issueKey={issueKey}
          onReply={onReply}
          root={item}
          titles={titles}
        />
      }
      turnID={item.id}
    />
  );
}

export function ConversationTab({
  focusItemId,
  isClosed,
  issueKey,
  route,
  state,
  visible,
}: ConversationTabProps): ReactNode {
  const queryClient = useQueryClient();
  const [failedOps, setFailedOps] = useState<FailedStateOperations>();
  const [retryingFailedOps, setRetryingFailedOps] = useState(false);
  const log = useInfiniteQuery({
    initialPageParam: null as number | null,
    queryKey: ["events", issueKey],
    queryFn: ({ pageParam }) =>
      api.getIssueEvents(
        issueKey,
        pageParam === null ? { limit: 200, order: "desc" } : { before: pageParam, limit: 200 }
      ),
    getNextPageParam: (page) => (page.length === 200 ? page.at(-1)?.seq : undefined),
  });
  const issueState = eventState(state, issueKey);
  const lastRead = useRef(issueState.last_read_seq);
  const observed = useRef(new Map<Element, number>());
  const timers = useRef(new Map<Element, number>());
  const events = useMemo(() => eventItems(log.data), [log.data]);
  const today = dateKey(new Date().toISOString());
  const items = useMemo(
    () => buildConversationItems({ events, lastReadSeq: issueState.last_read_seq, today }),
    [events, issueState.last_read_seq, today]
  );
  const [showActivity, setShowActivity] = useShowActivity();
  const [showRetracted, setShowRetracted] = useShowRetracted();
  const retractedCount = useMemo(() => countRetractedAsks(items), [items]);
  const shown = useMemo(
    () => visibleConversationItems(items, { showActivity, showRetracted }),
    [items, showActivity, showRetracted]
  );
  const [ownSendCount, setOwnSendCount] = useState(0);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [replyTo, setReplyTo] = useState<ReplyTarget | null>(null);
  const itemSeqs = useMemo(
    () => shown.flatMap((item) => ("lastSeq" in item ? [item.lastSeq] : [])),
    [shown]
  );
  // The focused turn may be a reply nested in a thread; its own `data-turn` is the anchor.
  const targetTurnId = useMemo(() => {
    if (focusItemId === undefined) return undefined;
    for (const item of shown) {
      if (item.kind === "ask" && item.ask.id === focusItemId) return item.id;
      if (item.kind === "comment" && item.event.payload.id === focusItemId) return item.id;
      if (item.kind === "message" || item.kind === "targeted-message") {
        if (item.event.payload.id === focusItemId) return item.id;
        const reply = item.replies.find((candidate) => candidate.event.payload.id === focusItemId);
        if (reply !== undefined) return reply.id;
      }
    }
    return undefined;
  }, [focusItemId, shown]);
  const follow = useFollowLatest({
    enabled: visible,
    itemSeqs,
    ownSendCount,
  });
  const { agents, error: envoyError, titles } = useAgents(visible, pickerOpen);
  const observedTurnKey = useMemo(
    () =>
      shown
        .filter((item) => "lastSeq" in item)
        .map((item) => `${item.id}:${item.lastSeq}`)
        .join(","),
    [shown]
  );
  const hasFailedOps = failedOps?.issueKey === issueKey;
  const focusItem = useRef<string | undefined>(undefined);
  const focusPageLoads = useRef(0);
  const focusedItem = useRef<string | undefined>(undefined);

  useEffect(() => {
    if (focusItem.current !== focusItemId) {
      focusItem.current = focusItemId;
      focusPageLoads.current = 0;
      focusedItem.current = undefined;
    }
  }, [focusItemId]);

  useEffect(() => {
    if (!visible || focusItemId === undefined || focusedItem.current === focusItemId) {
      return;
    }
    if (targetTurnId !== undefined) {
      const target = document.querySelector<HTMLElement>(`[data-turn="${targetTurnId}"]`);
      if (target !== null) {
        target.scrollIntoView({ block: "center" });
        focusedItem.current = focusItemId;
      }
      return;
    }
    if (log.hasNextPage && !log.isFetchingNextPage && focusPageLoads.current < maxFocusPageLoads) {
      focusPageLoads.current += 1;
      void log.fetchNextPage();
    }
  }, [
    focusItemId,
    log.fetchNextPage,
    log.hasNextPage,
    log.isFetchingNextPage,
    targetTurnId,
    visible,
  ]);

  useEffect(() => {
    lastRead.current = issueState.last_read_seq;
  }, [issueState.last_read_seq]);

  useEffect(() => {
    if (!visible || observedTurnKey === "") {
      return;
    }
    const visibleEvents = new Set<Element>();
    let active = true;
    const scheduleRead = (target: Element, sequence: number) => {
      if (
        !active ||
        !visibleEvents.has(target) ||
        sequence <= lastRead.current ||
        timers.current.has(target)
      ) {
        return;
      }
      const timer = window.setTimeout(() => {
        timers.current.delete(target);
        if (!active || !visibleEvents.has(target) || sequence <= lastRead.current) {
          return;
        }
        lastRead.current = sequence;
        void api
          .putIssueState(issueKey, { last_read_seq: sequence })
          .then((next) => {
            queryClient.setQueryData<UserState>(["user-state"], (current) => ({
              ...current,
              [issueKey]: next,
            }));
          })
          .catch(async () => {
            const current = await queryClient
              .fetchQuery({ queryKey: ["user-state"], queryFn: () => api.getMyState() })
              .catch(() => undefined);
            if (!active) {
              return;
            }
            lastRead.current = eventState(current, issueKey).last_read_seq;
            scheduleRead(target, sequence);
          });
      }, 1_000);
      timers.current.set(target, timer);
    };
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          const sequence = observed.current.get(entry.target);
          if (sequence === undefined) {
            continue;
          }
          if (!entry.isIntersecting) {
            visibleEvents.delete(entry.target);
            const currentTimer = timers.current.get(entry.target);
            if (currentTimer !== undefined) {
              window.clearTimeout(currentTimer);
              timers.current.delete(entry.target);
            }
            continue;
          }
          visibleEvents.add(entry.target);
          scheduleRead(entry.target, sequence);
        }
      },
      { threshold: 0.75 }
    );
    for (const [element] of observed.current) {
      if (!element.isConnected) {
        observed.current.delete(element);
        continue;
      }
      observed.current.set(element, Number(element.getAttribute("data-event-seq")));
      observer.observe(element);
    }
    return () => {
      active = false;
      observer.disconnect();
      for (const timer of timers.current.values()) {
        window.clearTimeout(timer);
      }
      timers.current.clear();
    };
  }, [issueKey, observedTurnKey, queryClient, visible]);

  const registerObserved = useCallback((element: HTMLElement | null) => {
    if (element !== null) {
      observed.current.set(element, Number(element.dataset.eventSeq));
    }
  }, []);

  const applyOptimisticOperations = (operations: PinStateOperation[]) => {
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      let next = eventState(current, issueKey);
      for (const operation of operations) {
        next = {
          ...next,
          dismissed: applyPinStateOperation(next.dismissed, operation),
        };
      }
      return { ...current, [issueKey]: next };
    });
  };

  const enqueuePinOperations = (operations: PinStateOperation[]) => {
    for (const operation of operations) {
      void stateWrites
        .enqueue(issueKey, operation, {
          fetchState: async (key) => eventState(await api.getMyState(), key),
          onDrained: (key, next) => {
            queryClient.setQueryData<UserState>(["user-state"], (current) => ({
              ...current,
              [key]: next,
            }));
            setFailedOps((current) => (current?.issueKey === key ? undefined : current));
            setRetryingFailedOps(false);
          },
          onError: (key, failedOperations, next) => {
            if (next === undefined) {
              void queryClient.invalidateQueries({ queryKey: ["user-state"] });
            } else {
              queryClient.setQueryData<UserState>(["user-state"], (current) => ({
                ...current,
                [key]: next,
              }));
            }
            setFailedOps({
              authoritativeState: next,
              issueKey: key,
              operations: failedOperations,
            });
            setRetryingFailedOps(false);
          },
          putState: (key, dismissed) => api.putIssueState(key, { dismissed }),
        })
        .catch(() => {});
    }
  };

  const updatePins = (
    operation: PinStateOperation | ((dismissed: string[]) => PinStateOperation)
  ) => {
    if (hasFailedOps) {
      return;
    }
    let nextOperation: PinStateOperation | undefined;
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      const currentIssueState = eventState(current, issueKey);
      nextOperation =
        typeof operation === "function" ? operation(currentIssueState.dismissed) : operation;
      return {
        ...current,
        [issueKey]: {
          ...currentIssueState,
          dismissed: applyPinStateOperation(currentIssueState.dismissed, nextOperation),
        },
      };
    });
    if (nextOperation !== undefined) {
      enqueuePinOperations([nextOperation]);
    }
  };

  const retryFailedOperations = () => {
    if (failedOps === undefined || failedOps.issueKey !== issueKey || retryingFailedOps) {
      return;
    }
    setRetryingFailedOps(true);
    applyOptimisticOperations(failedOps.operations);
    enqueuePinOperations(failedOps.operations);
  };

  const dismissFailedOperations = () => {
    if (failedOps === undefined || failedOps.issueKey !== issueKey || retryingFailedOps) {
      return;
    }
    const authoritativeState = failedOps.authoritativeState;
    if (authoritativeState === undefined) {
      void queryClient.invalidateQueries({ queryKey: ["user-state"] });
    } else {
      queryClient.setQueryData<UserState>(["user-state"], (current) => ({
        ...current,
        [issueKey]: authoritativeState,
      }));
    }
    setFailedOps(undefined);
  };

  if (log.isPending) {
    return <p className={textMutedOnCanvas}>Loading conversation…</p>;
  }
  if (log.isError) {
    return <p className={dangerText}>Could not load the conversation.</p>;
  }

  // The reader's turn keeps its viewport position across live updates - a turn arriving while
  // they browse history, a pin reflow, the failed-operations banner, the unread divider moving -
  // except while they follow the latest turn, where an arrival simply appears in place.
  return (
    <ViewportAnchor
      as="section"
      className="flex min-h-[60dvh] flex-col gap-3 pb-56 sm:pb-16 xl:pb-0"
      enabled={() => !follow.pinnedToTop()}
      item="data-event-seq"
      label="Conversation"
    >
      {hasFailedOps ? (
        <div className={`flex items-center gap-3 text-sm ${dangerText}`} role="alert">
          <p>Couldn't save pin — retry</p>
          <button
            className="font-medium underline disabled:cursor-not-allowed disabled:opacity-50"
            disabled={retryingFailedOps}
            onClick={retryFailedOperations}
            type="button"
          >
            Retry
          </button>
          <button
            className="underline disabled:cursor-not-allowed disabled:opacity-50"
            disabled={retryingFailedOps}
            onClick={dismissFailedOperations}
            type="button"
          >
            Close
          </button>
        </div>
      ) : null}
      {isClosed ? null : (
        <ConversationComposer
          agents={agents}
          issueKey={issueKey}
          onCancelReply={() => setReplyTo(null)}
          onPickerOpenChange={setPickerOpen}
          envoyError={envoyError}
          onSent={() => {
            setReplyTo(null);
            setOwnSendCount((count) => count + 1);
          }}
          replyTo={replyTo}
          route={route}
        />
      )}
      <div className="flex items-center justify-end gap-4">
        {retractedCount === 0 ? null : (
          <label className={`flex min-h-11 items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
            <input
              checked={showRetracted}
              className={`${checkboxAccent} min-h-11 min-w-11`}
              onChange={(event) => setShowRetracted(event.target.checked)}
              type="checkbox"
            />
            Show retracted ({retractedCount})
          </label>
        )}
        <label className={`flex min-h-11 items-center gap-2 text-sm ${textSecondaryOnCanvas}`}>
          <input
            checked={showActivity}
            className={`${checkboxAccent} min-h-11 min-w-11`}
            onChange={(event) => setShowActivity(event.target.checked)}
            type="checkbox"
          />
          Show activity
        </label>
      </div>
      {/* biome-ignore lint/a11y/noRedundantRoles: The Conversation DOM contract exposes its list role. */}
      <ol aria-label="Conversation turns" className="flex flex-col gap-1" role="list">
        {shown.map((item) => {
          if (item.kind === "day-divider" || item.kind === "unread-divider") {
            const label = item.kind === "day-divider" ? item.label : "New since you last read";
            return (
              // biome-ignore lint/a11y/useSemanticElements: A labeled list item is required by the Conversation DOM contract.
              // biome-ignore lint/a11y/useFocusableInteractive: A divider is descriptive, not an interactive control.
              <li
                aria-label={label}
                className={`my-2 flex items-center gap-3 text-xs font-semibold tracking-wide uppercase ${
                  item.kind === "unread-divider" ? linkText : textMutedOnCanvas
                }`}
                key={item.id}
                // biome-ignore lint/a11y: The role is required by the Conversation DOM contract.
                role="separator"
              >
                <span className={`h-px flex-1 ${newDividerLine}`} />
                {label}
                <span className={`h-px flex-1 ${newDividerLine}`} />
              </li>
            );
          }

          const pinned = isPinnedEvent(issueState.dismissed, item.pinEventId);
          const onPin = () =>
            updatePins((dismissed) => ({
              id: eventItemId({ id: item.pinEventId }),
              op: isPinnedEvent(dismissed, item.pinEventId) ? "unpin" : "pin",
            }));

          if (item.kind === "targeted-message") {
            return (
              <TargetedMessageTurn
                agents={agents}
                current={item.id === targetTurnId}
                currentTurnId={targetTurnId}
                isClosed={isClosed}
                issueKey={issueKey}
                item={item}
                key={item.id}
                onReply={setReplyTo}
                register={registerObserved}
                titles={titles}
              />
            );
          }
          if (item.kind === "message" || item.kind === "comment") {
            return (
              <MessageTurn
                agents={agents}
                author={resolveAuthor(item.author, titles)}
                issueKey={issueKey}
                current={item.id === targetTurnId}
                currentTurnId={targetTurnId}
                disabled={hasFailedOps}
                isClosed={isClosed}
                item={item}
                key={item.id}
                onPin={onPin}
                onReply={setReplyTo}
                pinned={pinned}
                register={registerObserved}
                titles={titles}
              />
            );
          }
          if (item.kind === "ask") {
            return (
              <li
                aria-current={item.id === targetTurnId ? "true" : undefined}
                className="my-2 flex gap-3"
                data-event-seq={item.lastSeq}
                data-turn={item.id}
                key={item.id}
                ref={registerObserved}
              >
                <div className="min-w-0 flex-1">
                  <AskCard ask={item.ask} thread="collapsed" />
                </div>
                <TurnPin disabled={hasFailedOps} onPin={onPin} pinned={pinned} />
              </li>
            );
          }
          return (
            <li
              className={`flex items-baseline gap-2 px-2 text-xs ${textMutedOnCanvas}`}
              data-event-seq={item.lastSeq}
              data-kind="activity"
              data-turn={item.id}
              key={item.id}
              ref={registerObserved}
            >
              <span className="font-medium">{resolveAuthor(item.author, titles).label}</span>
              <span>{item.description}</span>
              {item.event.type === "comment.created" ? (
                <Link
                  className={`underline ${linkText}`}
                  to={buildIssuePath({ id: item.event.payload.id, key: issueKey, kind: "comment" })}
                >
                  view
                </Link>
              ) : null}
              <span aria-hidden="true">·</span>
              <Timestamp at={item.at} />
            </li>
          );
        })}
      </ol>
      {shown.length === 0 ? <p className={textMutedOnCanvas}>No messages yet.</p> : null}
      {log.hasNextPage ? (
        <div className="flex items-center justify-center">
          <button
            className={`min-h-11 rounded-lg px-3 text-sm font-medium ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
            disabled={log.isFetchingNextPage}
            onClick={() => void log.fetchNextPage()}
            type="button"
          >
            {log.isFetchingNextPage ? "Loading…" : "Load older"}
          </button>
        </div>
      ) : null}
      {visible && !follow.atTop && shown.length > 0 ? (
        <button
          className={`fixed right-6 bottom-36 z-20 min-h-11 rounded-full px-4 text-sm font-semibold shadow-lg xl:bottom-24 ${primaryButtonBg} ${primaryButtonHoverBg}`}
          data-testid="jump-to-latest"
          onClick={follow.jumpToLatest}
          type="button"
        >
          Jump to latest{follow.newItemCount === 0 ? "" : ` · ${follow.newItemCount} new`}
        </button>
      ) : null}
    </ViewportAnchor>
  );
}

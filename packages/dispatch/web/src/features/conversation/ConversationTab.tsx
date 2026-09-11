import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import type { Event, UserIssueState, UserState } from "../../api/types";
import {
  checkboxAccent,
  dangerText,
  inputClasses,
  linkHoverText,
  linkText,
  newDividerLine,
  primaryButtonBg,
  primaryButtonHoverBg,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  surfaceMutedHoverBg,
  surfaceMutedStrongBg,
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
import { buildIssuePath } from "../refs/routes";
import { Timestamp } from "../refs/Timestamp";
import { type Author, resolveAuthor } from "./authors";
import { ConversationComposer } from "./ConversationComposer";
import {
  buildConversationItems,
  type ConversationItem,
  dateKey,
  visibleConversationItems,
} from "./conversation-model";
import { useFollowLatest } from "./use-follow-latest";
import { usePreserveReaderPosition } from "./use-preserve-reader-position";
import { useShowActivity } from "./use-show-activity";
import { useAgents } from "./useAgents";

const stateWrites = new IssueStateWriteQueue();

interface FailedStateOperations {
  authoritativeState: UserIssueState | undefined;
  issueKey: string;
  operations: PinStateOperation[];
}

interface ConversationTabProps {
  focusItemId?: string;
  issueKey: string;
  isClosed: boolean;
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

function Avatar({ author }: { author: Author }): ReactNode {
  return (
    <span
      aria-hidden="true"
      className={`grid h-8 w-8 shrink-0 place-items-center text-xs font-semibold ${
        author.shape === "round" ? "rounded-full" : "rounded-md"
      } ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`}
    >
      {author.initials}
    </span>
  );
}

function PinButton({
  disabled,
  onPin,
  pinned,
}: {
  disabled: boolean;
  onPin: () => void;
  pinned: boolean;
}): ReactNode {
  return (
    <button
      aria-label={pinned ? "Unpin" : "Pin"}
      className={`min-h-11 min-w-11 shrink-0 text-sm ${linkText} ${linkHoverText} disabled:cursor-not-allowed disabled:opacity-50`}
      disabled={disabled}
      onClick={onPin}
      type="button"
    >
      {pinned ? "Unpin" : "Pin"}
    </button>
  );
}

function MessageTurn({
  author,
  current,
  disabled,
  item,
  onPin,
  pinned,
  register,
}: {
  author: Author;
  current: boolean;
  disabled: boolean;
  item: Extract<ConversationItem, { kind: "message" | "comment" }>;
  onPin: () => void;
  pinned: boolean;
  register: (element: HTMLElement | null) => void;
}): ReactNode {
  return (
    <li
      aria-current={current ? "true" : undefined}
      className={`group flex gap-3 rounded-lg px-2 ${item.continued ? "py-0.5" : "mt-2 py-1"} ${surfaceMutedHoverBg}`}
      data-continued={String(item.continued)}
      data-event-seq={item.lastSeq}
      data-turn={item.id}
      ref={register}
    >
      {item.continued ? <span className="w-8 shrink-0" /> : <Avatar author={author} />}
      <div className="min-w-0 flex-1">
        {item.continued ? null : (
          <p className={`flex items-baseline gap-2 text-sm ${textSecondaryOnSurface}`}>
            <span className="font-semibold">{author.label}</span>
            <Timestamp at={item.at} />
          </p>
        )}
        <EventBody event={item.event} />
      </div>
      <PinButton disabled={disabled} onPin={onPin} pinned={pinned} />
    </li>
  );
}

export function ConversationTab({
  focusItemId,
  isClosed,
  issueKey,
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
  const shown = useMemo(() => visibleConversationItems(items, showActivity), [items, showActivity]);
  const [ownSendCount, setOwnSendCount] = useState(0);
  const itemSeqs = useMemo(
    () => shown.flatMap((item) => ("lastSeq" in item ? [item.lastSeq] : [])),
    [shown]
  );
  const targetTurnId = useMemo(
    () =>
      focusItemId === undefined
        ? undefined
        : shown.find(
            (item) =>
              (item.kind === "ask" && item.ask.id === focusItemId) ||
              (item.kind === "comment" && item.event.payload.id === focusItemId)
          )?.id,
    [focusItemId, shown]
  );
  const follow = useFollowLatest({
    enabled: visible,
    itemSeqs,
    ownSendCount,
  });
  const setSection = usePreserveReaderPosition(() => !follow.pinnedToTop());
  const { agents, titles } = useAgents(visible);
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

  return (
    <section
      aria-label="Conversation"
      className="flex min-h-[60dvh] flex-col gap-3 pb-16 xl:pb-0"
      ref={setSection}
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
          issueKey={issueKey}
          onSent={() => setOwnSendCount((count) => count + 1)}
          recipientSlot={
            <label className={`flex min-h-11 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
              To
              <select
                aria-label="Recipient"
                className={`min-h-11 max-w-48 rounded-lg border px-3 text-sm ${inputClasses(true)}`}
                defaultValue=""
              >
                <option value="">No recipient</option>
                {agents.map((agent) => (
                  <option key={agent.session_id} value={agent.session_id}>
                    {agent.title}
                  </option>
                ))}
              </select>
            </label>
          }
        />
      )}
      <div className="flex items-center justify-end">
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

          if (item.kind === "message" || item.kind === "comment") {
            return (
              <MessageTurn
                author={resolveAuthor(item.author, titles)}
                current={item.id === targetTurnId}
                disabled={hasFailedOps}
                item={item}
                key={item.id}
                onPin={onPin}
                pinned={pinned}
                register={registerObserved}
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
                  <AskCard ask={item.ask} events={events} thread="collapsed" />
                </div>
                <PinButton disabled={hasFailedOps} onPin={onPin} pinned={pinned} />
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
    </section>
  );
}

import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";

import { api } from "../../api/client";
import type { Event, UserIssueState, UserState } from "../../api/types";
import { actorLabel } from "../refs/actor";
import { buildLogItems, eventItemId, isPinnedEvent } from "./log-model";
import {
  applyDismissedStateOperation,
  type DismissedStateOperation,
  IssueStateWriteQueue,
} from "./state-write-queue";

const stateWrites = new IssueStateWriteQueue();

interface FailedStateOperations {
  authoritativeState: UserIssueState | undefined;
  issueKey: string;
  operations: DismissedStateOperation[];
}

function eventDescription(event: Event): string {
  if (event.type === "ask.answered") {
    const question = event.payload.question;
    return `Ask answered: ${typeof question === "string" ? question : "ask"}`;
  }
  if (event.type === "comment.resolved") {
    return "Comment resolved";
  }
  if (event.type === "message.created") {
    const body = event.payload.body;
    return typeof body === "string" ? body : "Message created";
  }
  if (event.type === "ask.opened") {
    const question = event.payload.question;
    return `Ask opened: ${typeof question === "string" ? question : "ask"}`;
  }
  if (event.type === "comment.created") {
    return "Comment created";
  }
  if (event.type === "issue.created") {
    return "Issue created";
  }
  if (event.type === "issue.updated") {
    return "Issue updated";
  }
  if (event.type === "issue.closed") {
    return "Issue closed";
  }
  if (event.type === "artifact.created") {
    return "Artifact created";
  }
  if (event.type === "artifact.version") {
    return "Artifact version saved";
  }
  if (event.type === "suggestion.accepted") {
    return "Suggestion accepted";
  }
  if (event.type === "suggestion.rejected") {
    return "Suggestion rejected";
  }
  return "Child status changed";
}

function eventItems(data: { pages: Event[][] } | undefined): Event[] {
  return data?.pages.flat() ?? [];
}

function eventState(state: UserState | undefined, issueKey: string): UserIssueState {
  return state?.[issueKey] ?? { dismissed: [], last_read_seq: 0, pinned: false };
}

export function LogTab({
  issueKey,
  state,
}: {
  issueKey: string;
  state: UserState | undefined;
}): ReactNode {
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
  const events = eventItems(log.data);
  const items = useMemo(
    () => buildLogItems(events, issueState.dismissed, issueState.last_read_seq),
    [events, issueState.dismissed, issueState.last_read_seq]
  );
  const visibleEventCount = items.filter((item) => item.kind === "event").length;
  const hasFailedOps = failedOps?.issueKey === issueKey;

  useEffect(() => {
    lastRead.current = issueState.last_read_seq;
  }, [issueState.last_read_seq]);

  useEffect(() => {
    if (visibleEventCount === 0) {
      return;
    }
    const visible = new Set<Element>();
    let active = true;
    const scheduleRead = (target: Element, sequence: number) => {
      if (
        !active ||
        !visible.has(target) ||
        sequence <= lastRead.current ||
        timers.current.has(target)
      ) {
        return;
      }
      const timer = window.setTimeout(() => {
        timers.current.delete(target);
        if (!active || !visible.has(target) || sequence <= lastRead.current) {
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
            visible.delete(entry.target);
            const currentTimer = timers.current.get(entry.target);
            if (currentTimer !== undefined) {
              window.clearTimeout(currentTimer);
              timers.current.delete(entry.target);
            }
            continue;
          }
          visible.add(entry.target);
          scheduleRead(entry.target, sequence);
        }
      },
      { threshold: 0.75 }
    );
    for (const [element] of observed.current) {
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
  }, [issueKey, queryClient, visibleEventCount]);

  const applyOptimisticOperations = (operations: DismissedStateOperation[]) => {
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      let next = eventState(current, issueKey);
      for (const operation of operations) {
        next = {
          ...next,
          dismissed: applyDismissedStateOperation(next.dismissed, operation),
        };
      }
      return { ...current, [issueKey]: next };
    });
  };

  const enqueueDismissedOperations = (operations: DismissedStateOperation[]) => {
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

  const updateDismissed = (
    operation: DismissedStateOperation | ((dismissed: string[]) => DismissedStateOperation)
  ) => {
    if (hasFailedOps) {
      return;
    }
    let nextOperation: DismissedStateOperation | undefined;
    queryClient.setQueryData<UserState>(["user-state"], (current) => {
      const issueState = eventState(current, issueKey);
      nextOperation = typeof operation === "function" ? operation(issueState.dismissed) : operation;
      return {
        ...current,
        [issueKey]: {
          ...issueState,
          dismissed: applyDismissedStateOperation(issueState.dismissed, nextOperation),
        },
      };
    });
    if (nextOperation !== undefined) {
      enqueueDismissedOperations([nextOperation]);
    }
  };

  const retryFailedOperations = () => {
    if (failedOps === undefined || failedOps.issueKey !== issueKey || retryingFailedOps) {
      return;
    }
    setRetryingFailedOps(true);
    applyOptimisticOperations(failedOps.operations);
    enqueueDismissedOperations(failedOps.operations);
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
    return <p className="text-slate-500">Loading log…</p>;
  }
  if (log.isError) {
    return <p className="text-rose-700">Could not load the issue log.</p>;
  }

  return (
    <section aria-label="Issue log" className="space-y-3">
      {hasFailedOps ? (
        <div className="flex items-center gap-3 text-sm text-rose-700" role="alert">
          <p>Couldn't save pin/dismiss — retry</p>
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
            Dismiss
          </button>
        </div>
      ) : null}
      {items.map((item) => {
        if (item.kind === "new-divider") {
          return (
            <div
              className="flex items-center gap-3 text-xs font-semibold tracking-wide text-sky-700 uppercase"
              key="new-divider"
            >
              <span className="h-px flex-1 bg-sky-200" />
              New
              <span className="h-px flex-1 bg-sky-200" />
            </div>
          );
        }
        return (
          <LogEvent
            disabled={hasFailedOps}
            event={item.event}
            folded={item.folded}
            key={item.event.id}
            onDismiss={() => updateDismissed({ id: eventItemId(item.event), op: "dismiss" })}
            onPin={() =>
              updateDismissed((dismissed) => ({
                id: eventItemId(item.event),
                op: isPinnedEvent(dismissed, item.event) ? "unpin" : "pin",
              }))
            }
            pinned={isPinnedEvent(issueState.dismissed, item.event)}
            register={(element) => {
              if (element === null) {
                return;
              }
              observed.current.set(element, item.event.seq);
            }}
          />
        );
      })}
      {items.length === 0 ? <p className="text-slate-500">No activity yet.</p> : null}
      {log.hasNextPage ? (
        <button
          className="rounded-lg border border-slate-300 px-3 py-2 text-sm font-medium text-slate-700 hover:border-sky-500"
          disabled={log.isFetchingNextPage}
          onClick={() => void log.fetchNextPage()}
          type="button"
        >
          {log.isFetchingNextPage ? "Loading…" : "Load older"}
        </button>
      ) : null}
    </section>
  );
}

function LogEvent({
  disabled,
  event,
  folded,
  onDismiss,
  onPin,
  pinned,
  register,
}: {
  disabled: boolean;
  event: Event;
  folded: boolean;
  onDismiss: () => void;
  onPin: () => void;
  pinned: boolean;
  register: (element: HTMLElement | null) => void;
}): ReactNode {
  return (
    <article
      className="rounded-xl border border-slate-200 bg-white p-4 shadow-sm"
      data-event-seq={event.seq}
      ref={register}
    >
      <div className="flex items-start justify-between gap-4">
        <div>
          <p className={folded ? "text-sm text-slate-600" : "font-medium text-slate-900"}>
            {eventDescription(event)}
          </p>
          <p className="mt-1 text-xs text-slate-500">
            {actorLabel(event.actor)} · {new Date(event.created_at).toLocaleString()}
          </p>
        </div>
        <div className="flex gap-2">
          <button
            className="text-sm text-sky-700 hover:text-sky-900"
            disabled={disabled}
            onClick={onPin}
            type="button"
          >
            {pinned ? "Unpin" : "Pin"}
          </button>
          <button
            className="text-sm text-slate-500 hover:text-slate-800"
            disabled={disabled}
            onClick={onDismiss}
            type="button"
          >
            Dismiss
          </button>
        </div>
      </div>
    </article>
  );
}

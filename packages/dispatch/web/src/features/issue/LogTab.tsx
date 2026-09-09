import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useMemo, useRef } from "react";

import { api } from "../../api/client";
import type { Event, UserIssueState, UserState } from "../../api/types";
import { buildLogItems, dismissEvent, isPinnedEvent, setEventPinned } from "./log-model";

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
  const log = useInfiniteQuery({
    initialPageParam: 0,
    queryKey: ["events", issueKey],
    queryFn: ({ pageParam }) => api.getIssueEvents(issueKey, { after: pageParam, limit: 200 }),
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

  useEffect(() => {
    lastRead.current = issueState.last_read_seq;
  }, [issueState.last_read_seq]);

  useEffect(() => {
    if (visibleEventCount === 0) {
      return;
    }
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          const sequence = observed.current.get(entry.target);
          if (sequence === undefined) {
            continue;
          }
          const currentTimer = timers.current.get(entry.target);
          if (!entry.isIntersecting) {
            if (currentTimer !== undefined) {
              window.clearTimeout(currentTimer);
              timers.current.delete(entry.target);
            }
            continue;
          }
          if (sequence <= lastRead.current || currentTimer !== undefined) {
            continue;
          }
          timers.current.set(
            entry.target,
            window.setTimeout(() => {
              if (sequence <= lastRead.current) {
                return;
              }
              lastRead.current = sequence;
              void api.putIssueState(issueKey, { last_read_seq: sequence }).then((next) => {
                queryClient.setQueryData<UserState>(["user-state"], (current) => ({
                  ...current,
                  [issueKey]: next,
                }));
              });
            }, 1_000)
          );
        }
      },
      { threshold: 0.75 }
    );
    for (const [element] of observed.current) {
      observer.observe(element);
    }
    return () => {
      observer.disconnect();
      for (const timer of timers.current.values()) {
        window.clearTimeout(timer);
      }
      timers.current.clear();
    };
  }, [issueKey, queryClient, visibleEventCount]);

  const updateDismissed = (dismissed: string[]) => {
    const optimistic = { ...issueState, dismissed };
    queryClient.setQueryData<UserState>(["user-state"], (current) => ({
      ...current,
      [issueKey]: optimistic,
    }));
    void api.putIssueState(issueKey, { dismissed }).then(
      (next) => {
        queryClient.setQueryData<UserState>(["user-state"], (current) => ({
          ...current,
          [issueKey]: next,
        }));
      },
      () => {
        void queryClient.invalidateQueries({ queryKey: ["user-state"] });
      }
    );
  };

  if (log.isPending) {
    return <p className="text-slate-500">Loading log…</p>;
  }
  if (log.isError) {
    return <p className="text-rose-700">Could not load the issue log.</p>;
  }

  return (
    <section aria-label="Issue log" className="space-y-3">
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
            event={item.event}
            folded={item.folded}
            key={item.event.id}
            onDismiss={() => updateDismissed(dismissEvent(issueState.dismissed, item.event))}
            onPin={() =>
              updateDismissed(
                setEventPinned(
                  issueState.dismissed,
                  item.event,
                  !isPinnedEvent(issueState.dismissed, item.event)
                )
              )
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
          {log.isFetchingNextPage ? "Loading…" : "Load more log events"}
        </button>
      ) : null}
    </section>
  );
}

function LogEvent({
  event,
  folded,
  onDismiss,
  onPin,
  pinned,
  register,
}: {
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
            {event.actor.id} · {new Date(event.created_at).toLocaleString()}
          </p>
        </div>
        <div className="flex gap-2">
          <button className="text-sm text-sky-700 hover:text-sky-900" onClick={onPin} type="button">
            {pinned ? "Unpin" : "Pin"}
          </button>
          <button
            className="text-sm text-slate-500 hover:text-slate-800"
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

export function pinnedLogItems(events: Event[], state: UserIssueState | undefined): Event[] {
  return events.filter((event) => isPinnedEvent(state?.dismissed ?? [], event));
}

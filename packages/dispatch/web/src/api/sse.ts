import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";

import type { Event, EventType } from "./types";

const eventTypes: EventType[] = [
  "issue.created",
  "issue.updated",
  "issue.closed",
  "artifact.created",
  "artifact.version",
  "ask.opened",
  "ask.answered",
  "comment.created",
  "comment.resolved",
  "suggestion.accepted",
  "suggestion.rejected",
  "message.created",
  "child.status",
];

export interface QueryInvalidator {
  invalidateQueries(filters: { queryKey: readonly unknown[] }): unknown;
}

function artifactId(event: Event): string | undefined {
  const id = event.payload.artifact_id;
  return typeof id === "string" ? id : undefined;
}

interface EventPages {
  pageParams: unknown[];
  pages: Event[][];
}

export function prependEventToLog(queryClient: QueryClient, event: Event): void {
  queryClient.setQueryData<EventPages>(["events", event.issue_key], (current) => {
    if (current === undefined) {
      return current;
    }
    const first = current.pages[0] ?? [];
    if (event.seq <= (first[0]?.seq ?? 0)) {
      return current;
    }
    return {
      ...current,
      pages: [[event, ...first], ...current.pages.slice(1)],
    };
  });
}

export function applyEventInvalidations(queryClient: QueryInvalidator, event: Event): void {
  queryClient.invalidateQueries({ queryKey: ["issue", event.issue_key] });
  queryClient.invalidateQueries({ queryKey: ["events", event.issue_key] });
  queryClient.invalidateQueries({ queryKey: ["issues"] });

  if (event.type.startsWith("issue.")) {
    return;
  }

  if (event.type === "artifact.created" || event.type === "artifact.version") {
    queryClient.invalidateQueries({ queryKey: ["artifacts", event.issue_key] });
    const id = artifactId(event);
    if (id !== undefined) {
      queryClient.invalidateQueries({ queryKey: ["artifact", id] });
    }
    queryClient.invalidateQueries({ queryKey: ["comments", event.issue_key] });
    queryClient.invalidateQueries({ queryKey: ["inbox"] });
    return;
  }

  if (event.type.startsWith("ask.")) {
    queryClient.invalidateQueries({ queryKey: ["asks", event.issue_key] });
    if (typeof event.payload.id === "string") {
      queryClient.invalidateQueries({ queryKey: ["ask", event.payload.id] });
    }
    queryClient.invalidateQueries({ queryKey: ["inbox"] });
    return;
  }

  if (event.type.startsWith("comment.") || event.type.startsWith("suggestion.")) {
    queryClient.invalidateQueries({ queryKey: ["comments", event.issue_key] });
    queryClient.invalidateQueries({ queryKey: ["artifact"] });
    return;
  }

  if (event.type === "message.created") {
    queryClient.invalidateQueries({ queryKey: ["messages", event.issue_key] });
    queryClient.invalidateQueries({ queryKey: ["artifact"] });
    return;
  }

  queryClient.invalidateQueries({ queryKey: ["children", event.issue_key] });
}

export function useEventStream(): void {
  const queryClient = useQueryClient();
  const lastId = useRef<number>(0);

  useEffect(() => {
    const stream = new EventSource(`/api/v1/events?since=${lastId.current}`);
    const receive = (message: MessageEvent<string>) => {
      const event = JSON.parse(message.data) as Event;
      const eventId = Number(message.lastEventId || event.id);
      if (Number.isFinite(eventId)) {
        lastId.current = eventId;
      }
      prependEventToLog(queryClient, event);
      applyEventInvalidations(queryClient, event);
    };

    for (const type of eventTypes) {
      stream.addEventListener(type, receive);
    }

    return () => {
      stream.close();
    };
  }, [queryClient]);
}

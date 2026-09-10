import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { applyEventInvalidations, prependEventToLog } from "../api/sse";
import type { Event } from "../api/types";

function event(
  type: Event["type"],
  payload: Record<string, unknown> = {},
  overrides: Partial<Event> = {}
): Event {
  return {
    actor: { kind: "user", id: "alice" },
    created_at: "2026-09-09T00:00:00Z",
    id: 12,
    issue_key: "CORE-1",
    notify: true,
    payload,
    seq: 4,
    type,
    ...overrides,
  } as Event;
}

test("ask events refresh the issue, its asks, selected ask, user state, and the inbox", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("ask.answered", { id: "ask-1" }));

  expect(invalidated).toEqual([
    ["issue", "CORE-1"],
    ["events", "CORE-1"],
    ["issues"],
    ["user-state"],
    ["inbox"],
    ["asks", "CORE-1"],
    ["ask", "ask-1"],
  ]);
});

test("issue changes refresh user state and the inbox", () => {
  for (const type of ["issue.closed", "issue.updated"] as const) {
    const invalidated: unknown[][] = [];
    const queryClient = {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        invalidated.push([...queryKey]);
        return Promise.resolve();
      },
    };

    applyEventInvalidations(queryClient, event(type));

    expect(invalidated).toEqual([
      ["issue", "CORE-1"],
      ["events", "CORE-1"],
      ["issues"],
      ["user-state"],
      ["inbox"],
    ]);
  }
});

test("artifact versions refresh the document and its anchored margin items", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("artifact.version", { artifact_id: "artifact-1" }));

  expect(invalidated).toEqual([
    ["issue", "CORE-1"],
    ["events", "CORE-1"],
    ["issues"],
    ["user-state"],
    ["inbox"],
    ["artifacts", "CORE-1"],
    ["artifact", "artifact-1"],
    ["comments", "CORE-1"],
  ]);
});

test("message events refresh the affected issue messages, artifact references, and the inbox", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("message.created"));

  expect(invalidated).toEqual([
    ["issue", "CORE-1"],
    ["events", "CORE-1"],
    ["issues"],
    ["user-state"],
    ["inbox"],
    ["messages", "CORE-1"],
    ["artifact"],
  ]);
});

test("comment events refresh artifact references and the inbox", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("comment.created"));

  expect(invalidated).toContainEqual(["artifact"]);
  expect(invalidated).toContainEqual(["inbox"]);
  expect(invalidated).toContainEqual(["user-state"]);
});

test("every event type refreshes user state so unread badges stay live across tabs", () => {
  for (const type of [
    "issue.created",
    "artifact.created",
    "ask.opened",
    "comment.resolved",
    "suggestion.accepted",
    "message.created",
    "child.status",
  ] as const) {
    const invalidated: unknown[][] = [];
    const queryClient = {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        invalidated.push([...queryKey]);
        return Promise.resolve();
      },
    };

    applyEventInvalidations(queryClient, event(type));

    expect(invalidated).toContainEqual(["user-state"]);
    expect(invalidated).toContainEqual(["inbox"]);
  }
});

test("a comment reply to an ask refreshes that ask's thread", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("comment.created", { ask_id: "ask-1" }));

  expect(invalidated).toContainEqual(["ask-thread", "ask-1"]);
});

test("SSE event prepends a newer event to the loaded log page", () => {
  const queryClient = new QueryClient();
  queryClient.setQueryData(["events", "CORE-1"], {
    pageParams: [null],
    pages: [[event("message.created", {}, { id: 7, seq: 7 })]],
  });

  prependEventToLog(queryClient, event("message.created", {}, { id: 8, seq: 8 }));

  expect(queryClient.getQueryData<{ pages: Event[][] }>(["events", "CORE-1"])?.pages[0]).toEqual([
    event("message.created", {}, { id: 8, seq: 8 }),
    event("message.created", {}, { id: 7, seq: 7 }),
  ]);
});

test("a resolved ask refreshes the inbox, issue count, and its reply thread", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("ask.resolved" as Event["type"], { id: "ask-1" }));

  expect(invalidated).toEqual([
    ["issue", "CORE-1"],
    ["events", "CORE-1"],
    ["issues"],
    ["user-state"],
    ["inbox"],
    ["asks", "CORE-1"],
    ["ask", "ask-1"],
    ["ask-thread", "ask-1"],
  ]);
});

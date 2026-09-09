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
  };
}

test("ask events refresh the issue, its asks, selected ask, and the inbox", () => {
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
    ["asks", "CORE-1"],
    ["ask", "ask-1"],
    ["inbox"],
  ]);
});

test("issue changes refresh the inbox", () => {
  for (const type of ["issue.closed", "issue.updated"] as const) {
    const invalidated: unknown[][] = [];
    const queryClient = {
      invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
        invalidated.push([...queryKey]);
        return Promise.resolve();
      },
    };

    applyEventInvalidations(queryClient, event(type));

    expect(invalidated).toEqual([["issue", "CORE-1"], ["events", "CORE-1"], ["issues"], ["inbox"]]);
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
    ["artifacts", "CORE-1"],
    ["artifact", "artifact-1"],
    ["comments", "CORE-1"],
    ["inbox"],
  ]);
});

test("message events refresh the affected issue messages and artifact references", () => {
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
    ["messages", "CORE-1"],
    ["artifact"],
  ]);
});

test("comment events refresh artifact references", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("comment.created"));

  expect(invalidated).toContainEqual(["artifact"]);
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

import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";

import { applyEventInvalidations, mergeEventPages, prependEventToLog } from "../api/sse";
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
    project: "CORE",
    seq: 4,
    type,
    ...overrides,
  } as Event;
}

function queryClient(keys: readonly (readonly unknown[])[]): QueryClient {
  const client = new QueryClient({
    defaultOptions: { queries: { staleTime: Number.POSITIVE_INFINITY } },
  });
  for (const key of keys) client.setQueryData(key, { loaded: true });
  return client;
}

function expectStale(client: QueryClient, key: readonly unknown[]): void {
  expect(client.getQueryCache().find({ queryKey: key, exact: true })?.isStale()).toBe(true);
}

function expectFresh(client: QueryClient, key: readonly unknown[]): void {
  expect(client.getQueryCache().find({ queryKey: key, exact: true })?.isStale()).toBe(false);
}

test("known message events refresh their detail and references without refetching workspace lists", () => {
  for (const type of ["message.created", "message.delivery", "message.answered"] as const) {
    const client = queryClient([
      ["issue", "CORE-1"],
      ["events", "CORE-1"],
      ["messages", "CORE-1"],
      ["references"],
      ["issues"],
      ["issues", "pinned"],
      ["inbox"],
      ["user-state"],
    ]);
    applyEventInvalidations(client, event(type));
    expectStale(client, ["issue", "CORE-1"]);
    expectStale(client, ["events", "CORE-1"]);
    expectStale(client, ["messages", "CORE-1"]);
    if (type === "message.delivery") expectFresh(client, ["references"]);
    else expectStale(client, ["references"]);
    expectFresh(client, ["issues"]);
    expectFresh(client, ["issues", "pinned"]);
    expectFresh(client, ["inbox"]);
    expectFresh(client, ["user-state"]);
  }
});

test("non-ask comments skip Inbox while retaining their detail and references", () => {
  const client = queryClient([
    ["issue", "CORE-1"],
    ["events", "CORE-1"],
    ["comments", "CORE-1"],
    ["comment", "comment-1"],
    ["references"],
    ["issues"],
    ["issues", "pinned"],
    ["inbox"],
    ["user-state"],
  ]);
  applyEventInvalidations(client, event("comment.created", { id: "comment-1" }));
  expectStale(client, ["issue", "CORE-1"]);
  expectStale(client, ["events", "CORE-1"]);
  expectStale(client, ["comments", "CORE-1"]);
  expectStale(client, ["comment", "comment-1"]);
  expectStale(client, ["references"]);
  expectFresh(client, ["issues"]);
  expectFresh(client, ["issues", "pinned"]);
  expectFresh(client, ["inbox"]);
  expectFresh(client, ["user-state"]);
});

test("ask replies and lifecycle events use the conservative invalidation baseline", () => {
  for (const incoming of [
    event("comment.created", { ask_id: "ask-1", ask_waiting_on: "human", id: "comment-1" }),
    event("ask.answered", { id: "ask-1" }),
    event("issue.closed"),
    event("architecture.synced", {}, { issue_key: null, project: "CORE" }),
  ]) {
    const client = queryClient([
      ["issues"],
      ["inbox"],
      ["user-state"],
      ["issue", "CORE-ROOT"],
      ["references"],
    ]);
    applyEventInvalidations(client, incoming);
    expectStale(client, ["issues"]);
    expectStale(client, ["inbox"]);
    expectStale(client, ["user-state"]);
    expectStale(client, ["issue", "CORE-ROOT"]);
    expectStale(client, ["references"]);
  }
});

test("SSE event prepends a newer event to the loaded log page", () => {
  const client = new QueryClient();
  client.setQueryData(["events", "CORE-1"], {
    pageParams: [null],
    pages: [[event("message.created", {}, { id: 7, seq: 7 })]],
  });
  prependEventToLog(client, event("message.created", {}, { id: 8, seq: 8 }));
  expect(client.getQueryData<{ pages: Event[][] }>(["events", "CORE-1"])?.pages[0]).toEqual([
    event("message.created", {}, { id: 8, seq: 8 }),
    event("message.created", {}, { id: 7, seq: 7 }),
  ]);
});

test("a log page that predates streamed events keeps them in front of it", () => {
  const seq = (n: number) => event("message.created", { body: `m${n}` }, { id: n, seq: n });
  expect(
    mergeEventPages(
      { pageParams: [null], pages: [[seq(9), seq(8), seq(7)]] },
      { pageParams: [null], pages: [[seq(7), seq(6)]] }
    )
  ).toEqual({ pageParams: [null], pages: [[seq(9), seq(8), seq(7), seq(6)]] });
});

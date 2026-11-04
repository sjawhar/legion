import { expect, test } from "bun:test";
import { QueryClient } from "@tanstack/react-query";
import { eventRefreshTargets } from "./event-refresh";
import { refreshQueries } from "./query-refresh";
import type { Event } from "./types";

test("a new comment refreshes the person page that can gain its mention", async () => {
  const client = new QueryClient();
  const key = ["person-nodes", "alice"];
  client.setQueryData(key, { pages: [] });
  const event = {
    id: 1,
    type: "comment.created",
    issue_key: "CORE-1",
    notify: true,
    seq: 1,
    actor: { kind: "user", id: "bob" },
    payload: {
      id: "comment",
      body: "dispatch://people/alice",
      author: { kind: "user", id: "bob" },
      issue_key: "CORE-1",
      created_at: "2026-11-04T09:00:00Z",
      artifact_name: "",
      anchor: null,
      ask_id: null,
      turn: null,
      reply_to: null,
      resolved: false,
      resolved_by: null,
      resolved_at: null,
      edited_at: null,
      suggestion: null,
      thread_logins: [],
    },
    created_at: "2026-11-04T09:00:00Z",
  } satisfies Event;
  refreshQueries(client, eventRefreshTargets(event, "alice"));
  expect(client.getQueryState(key)?.isInvalidated).toBe(true);
  client.clear();
});

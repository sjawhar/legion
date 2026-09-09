import { expect, test } from "bun:test";

import { applyEventInvalidations } from "../api/sse";
import type { Event } from "../api/types";

function event(type: Event["type"], payload: Record<string, unknown> = {}): Event {
  return {
    actor: { kind: "user", id: "alice" },
    created_at: "2026-09-09T00:00:00Z",
    id: 12,
    issue_key: "CORE-1",
    notify: true,
    payload,
    seq: 4,
    type,
  };
}

test("ask events refresh the issue, its asks, and the inbox", () => {
  const invalidated: unknown[][] = [];
  const queryClient = {
    invalidateQueries: ({ queryKey }: { queryKey: readonly unknown[] }) => {
      invalidated.push([...queryKey]);
      return Promise.resolve();
    },
  };

  applyEventInvalidations(queryClient, event("ask.answered", { id: "ask-1" }));

  expect(invalidated).toEqual([["issue", "CORE-1"], ["asks", "CORE-1"], ["inbox"]]);
});

test("artifact versions refresh the issue and the artifact collection", () => {
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
    ["artifacts", "CORE-1"],
    ["artifact", "artifact-1"],
  ]);
});

test("message events refresh only the affected issue messages", () => {
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
    ["messages", "CORE-1"],
  ]);
});

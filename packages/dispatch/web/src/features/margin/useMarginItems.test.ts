import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";

import type { Artifact, Comment, Event } from "../../api/types";
import {
  anchoredThreadComments,
  fetchPinnedEvents,
  marginItemId,
  useMarginItems,
} from "./useMarginItems";

function event(id: string): Event {
  const seq = Number(id);
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-09T00:00:00Z",
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: { id: "alice", kind: "user" },
      body: "Update",
      created_at: "2026-09-09T00:00:00Z",
      id: `message-${id}`,
      issue_key: "CORE-1",
    },
    seq,
    type: "message.created",
  };
}

const artifact: Artifact = {
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "doc",
  name: "spec.md",
  primary: true,
  slug: "spec",
  versions: [],
};

function anchorOn(artifactId: string, quote = "anchored", orphaned = false) {
  return {
    artifact_id: artifactId,
    mark_id: `mark-${artifactId}`,
    orphaned,
    quote,
    version: 1,
  };
}

function comment(id: string, markId: string | null, createdAt: string, orphaned = false): Comment {
  return {
    anchor:
      markId === null
        ? null
        : {
            artifact_id: artifact.id,
            mark_id: markId,
            orphaned,
            quote: "selected",
            version: 1,
          },
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: id,
    created_at: createdAt,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_by: null,
    resolved_at: null,
    edited_at: null,
    suggestion: null,
  };
}

test("pinned lookup chunks 51 ids and combines results by sequence", async () => {
  const ids = Array.from({ length: 51 }, (_, index) => String(index + 1));
  const requests: string[][] = [];

  const events = await fetchPinnedEvents(
    async (_issueKey, options) => {
      const requestedIds = options.ids ?? [];
      requests.push(requestedIds);
      return requestedIds.map(event);
    },
    "CORE-1",
    ids
  );

  expect(requests).toEqual([ids.slice(0, 50), ids.slice(50)]);
  expect(events.map(({ seq }) => seq)).toEqual(ids.map(Number));
});

test("unanchored comments and their replies leave the margin; replies to anchored roots stay", () => {
  const root = { ...comment("r", "root", "2026-09-09T00:00:00Z"), anchor: anchorOn("a1") };
  const replyToRoot = {
    ...comment("r1", null, "2026-09-09T00:01:00Z"),
    reply_to: root.id,
  };
  const loose = comment("u", null, "2026-09-09T00:02:00Z");
  const replyToLoose = {
    ...comment("u1", null, "2026-09-09T00:03:00Z"),
    reply_to: loose.id,
  };
  const other = { ...comment("o", "other", "2026-09-09T00:04:00Z"), anchor: anchorOn("a2") };

  expect(
    anchoredThreadComments([root, replyToRoot, loose, replyToLoose, other], "a1").map(
      (comment) => comment.id
    )
  ).toEqual(["r", "r1"]);
});

function OrderedItems() {
  const { items } = useMarginItems(
    "CORE-1",
    "comments",
    artifact,
    new Map([
      ["m-b", 4],
      ["m-a", 12],
    ])
  );
  return createElement(
    "output",
    { "aria-label": "Margin item order" },
    items.map(marginItemId).join(",")
  );
}

test("useMarginItems orders found and missing document anchors while excluding unanchored comments", () => {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["issue", "CORE-1"], {
    artifacts: [artifact],
    primary_artifact_id: artifact.id,
  });
  queryClient.setQueryData(["asks", "CORE-1"], []);
  queryClient.setQueryData(
    ["comments", "CORE-1"],
    [
      comment("A", "m-a", "2026-09-09T00:00:00Z"),
      comment("B", "m-b", "2026-09-09T00:01:00Z"),
      comment("C", "m-c", "2026-09-09T00:02:00Z", true),
      comment("D", null, "2026-09-09T00:03:00Z"),
    ]
  );
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});

  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: ["/issues/CORE-1/spec"] },
      createElement(QueryClientProvider, { client: queryClient }, createElement(OrderedItems))
    )
  );

  try {
    expect(screen.getByLabelText("Margin item order").textContent).toBe("B,A,C");
  } finally {
    view.unmount();
  }
});

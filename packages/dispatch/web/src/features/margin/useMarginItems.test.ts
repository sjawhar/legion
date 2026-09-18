import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { api } from "../../api/client";
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
      deliveries: [],
      id: `message-${id}`,
      in_reply_to: null,
      issue_key: "CORE-1",
      target: null,
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
    block_id: null,
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
            block_id: null,
            mark_id: markId,
            orphaned,
            quote: "selected",
            version: 1,
          },
    ask_id: null,
    turn: null,
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
    ...commentDeliveryFields(),
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

test("does not present unanchored comments while the issue artifact is loading", () => {
  const loose = comment("unanchored", null, "2026-09-09T00:00:00Z");

  expect(anchoredThreadComments([loose], undefined)).toEqual([]);
});

function OrderedItems() {
  const { items } = useMarginItems(
    { key: "CORE-1", kind: "issue" },
    "comments",
    artifact,
    new Map([
      ["m-b", { pos: 4, top: 40 }],
      ["m-a", { pos: 12, top: 72 }],
    ]),
    new Map(),
    undefined
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

function ThreadItems() {
  const { resolvedThreads, threads } = useMarginItems(
    { key: "CORE-1", kind: "issue" },
    "comments",
    artifact,
    new Map(),
    new Map(),
    undefined
  );
  return createElement(
    "output",
    { "aria-label": "Margin thread groups" },
    [...threads, ...resolvedThreads]
      .map((thread) => `${thread.key}:${thread.replies.map((reply) => reply.id).join(",")}`)
      .join("|")
  );
}

test("useMarginItems groups replies flat under their root and separates resolved roots", () => {
  const root = comment("root", "mark-root", "2026-09-09T00:00:00Z");
  const reply = {
    ...comment("reply", null, "2026-09-09T00:01:00Z"),
    reply_to: root.id,
  };
  const deeperReply = {
    ...comment("deeper", null, "2026-09-09T00:02:00Z"),
    reply_to: reply.id,
  };
  const resolved = {
    ...comment("resolved", "mark-resolved", "2026-09-09T00:03:00Z"),
    resolved: true,
  };
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", "CORE-1"], [root, reply, deeperReply, resolved]);

  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: ["/issues/CORE-1/spec"] },
      createElement(QueryClientProvider, { client: queryClient }, createElement(ThreadItems))
    )
  );

  try {
    expect(screen.getByLabelText("Margin thread groups").textContent).toBe(
      "root:reply,deeper|resolved:"
    );
  } finally {
    view.unmount();
  }
});
function DocumentItems() {
  const items = useMarginItems(
    {
      artifactId: "artifact-1",
      kind: "document",
      project: "CORE",
      slug: "design-notes",
    },
    "comments",
    { ...artifact, issue_key: null, primary: false, slug: "design-notes" },
    new Map(),
    new Map(),
    undefined
  );
  return createElement(
    "output",
    { "aria-label": "Document margin state" },
    JSON.stringify({
      closed: items.isClosed,
      itemIds: items.items.map(marginItemId),
      pinned: items.pinnedIds,
    })
  );
}

test("a document owner loads asks and comments from the artifact routes, hides pinned, and is never closed", async () => {
  const documentComment = {
    ...comment("comment-1", "m-1", "2026-09-09T00:00:00Z"),
    issue_key: null,
  };
  const listArtifactAsks = spyOn(api, "listArtifactAsks").mockResolvedValue([
    {
      anchor: {
        artifact_id: "artifact-1",
        block_id: null,
        mark_id: "ask-mark",
        orphaned: false,
        quote: "Design",
        version: 1,
      },
      answer: null,
      artifact_id: "artifact-1",
      author: { id: "session-1", kind: "session" as const },
      created_at: "2026-09-09T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      issue_key: null,
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [],
      question: "Should this ship?",
      state: "open" as const,
      urgency: "med" as const,
    },
  ]);
  const listArtifactComments = spyOn(api, "listArtifactComments").mockResolvedValue([
    documentComment,
  ]);
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([]);
  const getMyState = spyOn(api, "getMyState").mockResolvedValue({});
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: ["/projects/CORE/documents/design-notes"] },
      createElement(QueryClientProvider, { client: queryClient }, createElement(DocumentItems))
    )
  );

  try {
    await waitFor(() => expect(listArtifactAsks).toHaveBeenCalledWith("artifact-1", "all"));
    await waitFor(() => expect(listArtifactComments).toHaveBeenCalledWith("artifact-1"));
    await waitFor(() =>
      expect(screen.getByLabelText("Document margin state").textContent).toContain("comment-1")
    );
    expect(screen.getByLabelText("Document margin state").textContent).toContain('"closed":false');
    expect(screen.getByLabelText("Document margin state").textContent).toContain('"pinned":[]');
    expect(getMyState).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    getInbox.mockRestore();
    getMyState.mockRestore();
    listArtifactAsks.mockRestore();
    listArtifactComments.mockRestore();
  }
});

function ActionButtons() {
  const { mutateItem, pendingActionIds, actionErrorId } = useMarginItems(
    { key: "CORE-1", kind: "issue" },
    "comments",
    artifact,
    new Map(),
    new Map(),
    undefined
  );
  return createElement(
    "div",
    null,
    createElement(
      "output",
      { "aria-label": "Action state" },
      JSON.stringify({ error: actionErrorId ?? null, pending: [...pendingActionIds] })
    ),
    ...["A", "B", "C"].map((id) =>
      createElement(
        "button",
        { key: id, onClick: () => mutateItem({ id, kind: "accept" }), type: "button" },
        `Accept ${id}`
      )
    )
  );
}

function renderActionButtons() {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", "CORE-1"], {
    artifacts: [artifact],
    primary_artifact_id: artifact.id,
  });
  queryClient.setQueryData(["asks", "CORE-1"], []);
  queryClient.setQueryData(["comments", "CORE-1"], []);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["user-state"], {});
  const view = render(
    createElement(
      MemoryRouter,
      { initialEntries: ["/issues/CORE-1/spec"] },
      createElement(QueryClientProvider, { client: queryClient }, createElement(ActionButtons))
    )
  );
  return { queryClient, unmount: view.unmount };
}

/** One held `acceptComment` call: the test settles it when it chooses. */
interface PendingSave {
  promise: Promise<Comment>;
  reject: (reason: unknown) => void;
  resolve: (value: Comment) => void;
}

function actionState(): { error: string | null; pending: string[] } {
  return JSON.parse(screen.getByLabelText("Action state").textContent ?? "");
}

test("two accepts in one tick reach the server one after the other, in click order", async () => {
  const saves: PendingSave[] = [];
  const acceptComment = spyOn(api, "acceptComment").mockImplementation(() => {
    const save = Promise.withResolvers<Comment>();
    saves.push(save);
    return save.promise;
  });
  const { unmount } = renderActionButtons();
  try {
    fireEvent.click(screen.getByRole("button", { name: "Accept A" }));
    fireEvent.click(screen.getByRole("button", { name: "Accept B" }));
    // A second press of the same button before its answer is one request, not two.
    fireEvent.click(screen.getByRole("button", { name: "Accept A" }));
    await waitFor(() => expect(acceptComment).toHaveBeenCalledTimes(1));
    expect(acceptComment).toHaveBeenLastCalledWith("A");
    await waitFor(() => expect(actionState().pending).toEqual(["A", "B"]));

    await act(async () => {
      saves[0]?.resolve(comment("A", "m-a", "2026-09-09T00:00:00Z"));
      await saves[0]?.promise;
    });
    await waitFor(() => expect(acceptComment).toHaveBeenCalledTimes(2));
    expect(acceptComment).toHaveBeenLastCalledWith("B");
    await waitFor(() => expect(actionState().pending).toEqual(["B"]));

    await act(async () => {
      saves[1]?.resolve(comment("B", "m-b", "2026-09-09T00:01:00Z"));
      await saves[1]?.promise;
    });
    await waitFor(() => expect(actionState()).toEqual({ error: null, pending: [] }));
    expect(acceptComment).toHaveBeenCalledTimes(2);
  } finally {
    unmount();
    acceptComment.mockRestore();
  }
});

test("a failed action drops the clicks queued behind it and shows its error", async () => {
  const saves: PendingSave[] = [];
  const acceptComment = spyOn(api, "acceptComment").mockImplementation(() => {
    const save = Promise.withResolvers<Comment>();
    saves.push(save);
    return save.promise;
  });
  const { unmount } = renderActionButtons();
  try {
    fireEvent.click(screen.getByRole("button", { name: "Accept A" }));
    fireEvent.click(screen.getByRole("button", { name: "Accept B" }));
    fireEvent.click(screen.getByRole("button", { name: "Accept C" }));
    await waitFor(() => expect(actionState().pending).toEqual(["A", "B", "C"]));

    await act(async () => {
      saves[0]?.reject(new Error("offline"));
      await saves[0]?.promise.catch(() => undefined);
    });
    await waitFor(() => expect(actionState()).toEqual({ error: "A", pending: [] }));
    expect(acceptComment).toHaveBeenCalledTimes(1);

    // The queue is gone, not stuck: the next click goes straight out.
    fireEvent.click(screen.getByRole("button", { name: "Accept C" }));
    await waitFor(() => expect(acceptComment).toHaveBeenCalledTimes(2));
    expect(acceptComment).toHaveBeenLastCalledWith("C");
  } finally {
    unmount();
    acceptComment.mockRestore();
  }
});

test("a chained action that fails refetches the comments its predecessor's success could not", async () => {
  const saves: PendingSave[] = [];
  const acceptComment = spyOn(api, "acceptComment").mockImplementation(() => {
    const save = Promise.withResolvers<Comment>();
    saves.push(save);
    return save.promise;
  });
  const { queryClient, unmount } = renderActionButtons();
  const invalidate = spyOn(queryClient, "invalidateQueries");
  const commentRefetches = () =>
    invalidate.mock.calls.filter(
      ([filters]) => JSON.stringify(filters?.queryKey) === JSON.stringify(["comments", "CORE-1"])
    ).length;
  try {
    fireEvent.click(screen.getByRole("button", { name: "Accept A" }));
    fireEvent.click(screen.getByRole("button", { name: "Accept B" }));
    await waitFor(() => expect(actionState().pending).toEqual(["A", "B"]));
    await act(async () => {
      saves[0]?.resolve(comment("A", "m-a", "2026-09-09T00:00:00Z"));
      await saves[0]?.promise;
    });
    await waitFor(() => expect(acceptComment).toHaveBeenCalledTimes(2));
    // A's success asked for a refetch; B's optimistic update cancelled it.
    const before = commentRefetches();
    expect(before).toBeGreaterThanOrEqual(1);

    await act(async () => {
      saves[1]?.reject(new Error("offline"));
      await saves[1]?.promise.catch(() => undefined);
    });
    await waitFor(() => expect(actionState()).toEqual({ error: "B", pending: [] }));
    expect(commentRefetches()).toBe(before + 1);
  } finally {
    unmount();
    invalidate.mockRestore();
    acceptComment.mockRestore();
  }
});

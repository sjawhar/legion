import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import type { ComponentProps } from "react";

import type { Comment } from "../../api/types";
import { ThreadList } from "./ThreadList";
import type { Thread } from "./useMarginItems";

function comment(id: string, body: string, resolved = false): Comment {
  return {
    anchor: {
      artifact_id: "artifact-1",
      mark_id: `${id}-mark`,
      orphaned: false,
      quote: "selected text",
      version: 1,
    },
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved,
    resolved_at: resolved ? "2026-09-10T00:02:00Z" : null,
    resolved_by: resolved ? { id: "alice", kind: "user" } : null,
    suggestion: null,
  };
}

function thread(id: string, resolved = false): Thread {
  const root = comment(id, `Comment ${id}`, resolved);
  return {
    anchor: root.anchor,
    key: root.id,
    lastReplyAt: undefined,
    replies: [],
    resolved,
    root: { comment: root, kind: "comment" },
  };
}
function renderList(overrides: Partial<ComponentProps<typeof ThreadList>> = {}) {
  const open = thread("open");
  const resolved = thread("resolved", true);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <ThreadList
        actionErrorId={undefined}
        artifactSlug="spec"
        expandedThreadKey={open.key}
        hoveredMarkId="open-mark"
        hoveredItemId={undefined}
        isClosed={false}
        owner={{ key: "CORE-1", kind: "issue" }}
        markPlacements={new Map([["open-mark", { pos: 5, top: 180 }]])}
        onAction={() => {}}
        onEdit={async () => undefined}
        onRetryAction={() => {}}
        onSelect={() => {}}
        onToggle={() => {}}
        onToggleResolved={() => {}}
        pendingActionId={undefined}
        resolvedThreads={[resolved]}
        showResolved={false}
        threads={[open]}
        viewerLogin="alice"
        {...overrides}
      />
    </QueryClientProvider>
  );
}

test("an anchored thread aligns to its mark and marks the hovered card", () => {
  const view = renderList();
  try {
    const card = screen.getByTestId("margin-comment-open");
    expect(card.parentElement?.style.top).toBe("180px");
    expect(card.getAttribute("data-hovered")).toBe("true");
  } finally {
    view.unmount();
  }
});

test("Resolved (N) reveals resolved threads only when the toggle is on", () => {
  let showResolved = false;
  const view = renderList({
    onToggleResolved: () => {
      showResolved = true;
    },
  });
  try {
    expect(screen.queryByRole("region", { name: "Resolved" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Resolved (1)" }));
    expect(showResolved).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a toggled resolved section contains resolved comment threads", () => {
  const view = renderList({ showResolved: true });
  try {
    expect(screen.getByRole("region", { name: "Resolved" })).not.toBeNull();
    expect(screen.getByTestId("margin-comment-resolved")).not.toBeNull();
  } finally {
    view.unmount();
  }
});

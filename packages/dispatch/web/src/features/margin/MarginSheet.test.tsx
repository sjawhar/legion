import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import type { Artifact, Comment } from "../../api/types";

import { MarginSheet } from "./MarginSheet";

const specArtifact: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  project: "CORE",
  kind: "doc",
  name: "Spec",
  primary: true,
  slug: "spec",
  versions: [],
};

test("MarginSheet renders its tab and open ask count from its model", () => {
  const view = render(
    <MarginSheet
      model={{
        actions: {
          closeComposer: () => {},
          onAction: () => {},
          onComposerSaved: () => {},
          onEdit: async () => undefined,
          onRetryAction: () => {},
          onRetryAnsweredAsk: undefined,
          onRetryComments: () => {},
          onRetryIssue: () => {},
          onToggleResolved: () => {},
          onToggleThread: () => {},
          onEditingChange: () => {},
        },
        composer: undefined,
        items: {
          actionErrorId: undefined,
          answeredAsksPending: false,
          asksPending: false,
          commentsPending: false,
          commentsError: false,
          historicalAsks: [],
          marginRef: { current: null },
          isClosed: false,
          owner: { key: "CORE-1", kind: "issue" },
          issuePending: false,
          openAskCount: 3,
          onSelectCard: () => {},
          needsYou: [],
          issueError: false,
          pinned: [],
          pendingActionId: undefined,
          pinnedIds: [],
          resolvedThreads: [],
          threads: [],
          viewerLogin: "alice",
          visibleArtifact: specArtifact,
        },
        placement: {
          blockPlacements: new Map(),
          markPlacements: new Map(),
        },
        selection: {
          expandedThreadKey: undefined,
          editingCommentId: undefined,
          hoveredItemId: undefined,
          hoveredMarkId: undefined,
          selectedItemId: undefined,
          showResolved: false,
        },
        sheet: {
          closeThread: () => {},
          expanded: false,
          threadKey: undefined,
          toggle: () => {},
        },
        filter: { blockId: undefined, clear: () => {} },
        tab: {
          set: () => {},
          value: "comments",
        },
      }}
    />
  );

  try {
    expect(screen.getByRole("button", { name: "Open review panel (3 open asks)" })).not.toBeNull();
    const margin = screen.getByTestId("margin-sheet");
    expect(margin.className).toContain("xl:sticky");
    expect(margin.className).toContain("xl:top-0");
    expect(margin.className).toContain("xl:h-auto");
    expect(margin.className).toContain("xl:max-h-dvh");
    expect(margin.className).toContain("xl:overflow-y-auto");
    const reviewItems = screen.getByLabelText("Margin review items");
    expect(reviewItems.className).not.toContain("max-h-");
    expect(reviewItems.className).not.toContain("overflow-y-auto");
    expect(screen.getByRole("tab", { name: "Comments" }).getAttribute("aria-selected")).toBe(
      "true"
    );
    expect(
      within(view.container)
        .getAllByRole("tab")
        .map((tab) => tab.textContent)
    ).toEqual(["Comments", "Pinned"]);
    expect(within(view.container).queryByRole("tab", { name: "Artifacts" })).toBeNull();
  } finally {
    view.unmount();
  }
});
test("a document owner shows the Comments tab only and no message composer", () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MarginSheet
        model={{
          actions: {
            closeComposer: () => {},
            onAction: () => {},
            onComposerSaved: () => {},
            onEdit: async () => undefined,
            onRetryAction: () => {},
            onRetryAnsweredAsk: undefined,
            onRetryComments: () => {},
            onRetryIssue: () => {},
            onToggleResolved: () => {},
            onToggleThread: () => {},
            onEditingChange: () => {},
          },
          composer: { anchor: undefined, kind: "message" },
          items: {
            actionErrorId: undefined,
            answeredAsksPending: false,
            asksPending: false,
            commentsError: false,
            commentsPending: false,
            historicalAsks: [],
            isClosed: false,
            issueError: false,
            issuePending: false,
            marginRef: { current: null },
            needsYou: [],
            onSelectCard: () => {},
            openAskCount: 0,
            owner: {
              artifactId: "artifact-1",
              kind: "document",
              project: "CORE",
              slug: "design-notes",
            },
            pendingActionId: undefined,
            pinned: [],
            pinnedIds: [],
            resolvedThreads: [],
            threads: [],
            viewerLogin: "alice",
            visibleArtifact: {
              ...specArtifact,
              issue_key: null,
              primary: false,
              slug: "design-notes",
            },
          },
          placement: { blockPlacements: new Map(), markPlacements: new Map() },
          selection: {
            expandedThreadKey: undefined,
            editingCommentId: undefined,
            hoveredItemId: undefined,
            hoveredMarkId: undefined,
            selectedItemId: undefined,
            showResolved: false,
          },
          sheet: { closeThread: () => {}, expanded: true, threadKey: undefined, toggle: () => {} },
          filter: { blockId: undefined, clear: () => {} },
          tab: { set: () => {}, value: "comments" },
        }}
      />
    </QueryClientProvider>
  );

  try {
    expect(
      within(view.container)
        .getAllByRole("tab")
        .map((tab) => tab.textContent)
    ).toEqual(["Comments"]);
    expect(screen.queryByRole("form", { name: "Message composer" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("MarginSheet shows the selected phone thread in a full-height view with a Back button", () => {
  const root: Comment = {
    anchor: {
      artifact_id: specArtifact.id,
      block_id: null,
      mark_id: "mark-1",
      orphaned: false,
      quote: "selected text",
      version: 1,
    },
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body: "Review this paragraph.",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id: "comment-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
  };
  const originalMatchMedia = window.matchMedia;
  window.matchMedia = (() =>
    ({
      addEventListener: () => {},
      addListener: () => {},
      dispatchEvent: () => true,
      matches: true,
      media: "",
      onchange: null,
      removeEventListener: () => {},
      removeListener: () => {},
    }) as MediaQueryList) as typeof window.matchMedia;
  let closed = false;
  let sheetClosed = false;
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MarginSheet
        model={{
          actions: {
            closeComposer: () => {},
            onAction: () => {},
            onComposerSaved: () => {},
            onEdit: async () => undefined,
            onRetryAction: () => {},
            onRetryAnsweredAsk: undefined,
            onRetryComments: () => {},
            onRetryIssue: () => {},
            onToggleResolved: () => {},
            onToggleThread: () => {},
            onEditingChange: () => {},
          },
          composer: undefined,
          items: {
            actionErrorId: undefined,
            answeredAsksPending: false,
            asksPending: false,
            commentsError: false,
            commentsPending: false,
            historicalAsks: [],
            isClosed: false,
            issueError: false,
            owner: { key: "CORE-1", kind: "issue" },
            issuePending: false,
            marginRef: { current: null },
            needsYou: [],
            onSelectCard: () => {},
            openAskCount: 0,
            pendingActionId: undefined,
            pinned: [],
            pinnedIds: [],
            resolvedThreads: [],
            threads: [
              {
                anchor: root.anchor,
                key: root.id,
                lastReplyAt: undefined,
                replies: [],
                resolved: false,
                root: { comment: root, kind: "comment" },
              },
            ],
            viewerLogin: "alice",
            visibleArtifact: specArtifact,
          },
          placement: { blockPlacements: new Map(), markPlacements: new Map() },
          selection: {
            expandedThreadKey: root.id,
            editingCommentId: undefined,
            hoveredItemId: undefined,
            hoveredMarkId: undefined,
            selectedItemId: root.id,
            showResolved: false,
          },
          sheet: {
            closeThread: () => {
              closed = true;
            },
            expanded: true,
            threadKey: root.id,
            toggle: (expanded) => {
              sheetClosed = expanded === false;
            },
          },
          filter: { blockId: undefined, clear: () => {} },
          tab: { set: () => {}, value: "comments" },
        }}
      />
    </QueryClientProvider>
  );

  try {
    const thread = screen.getByRole("dialog", { name: "Thread" });
    expect(thread.className).toContain("inset-0");
    expect(within(thread).getByTestId(`margin-comment-${root.id}`)).not.toBeNull();
    fireEvent.keyDown(thread, { key: "Escape" });
    expect(closed).toBe(true);
    expect(sheetClosed).toBe(false);
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
  }
});

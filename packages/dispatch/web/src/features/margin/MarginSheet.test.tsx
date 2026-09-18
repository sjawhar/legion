import { expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import type { Artifact, Comment } from "../../api/types";
import { KeymapProvider } from "../shell/KeymapProvider";
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
          pendingActionIds: new Set<string>(),
          pinnedIds: [],
          resolvedThreads: [],
          retractedAskCount: 0,
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
test("a document owner shows the Comments tab and comment composer only", () => {
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
          composer: { anchor: undefined, kind: "comment" },
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
            pendingActionIds: new Set<string>(),
            pinned: [],
            pinnedIds: [],
            resolvedThreads: [],
            retractedAskCount: 0,
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
    expect(screen.getByRole("form", { name: "Comment composer" })).toBeTruthy();
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
    turn: null,
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
    ...commentDeliveryFields(),
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
      <KeymapProvider>
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
              pendingActionIds: new Set<string>(),
              pinned: [],
              pinnedIds: [],
              resolvedThreads: [],
              retractedAskCount: 0,
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
      </KeymapProvider>
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

test("the phone sheet's summary rows show each suggestion's diff and Accept/Reject without opening the thread", () => {
  const suggestion = (id: string, replaceWith: string): Comment => ({
    anchor: {
      artifact_id: specArtifact.id,
      block_id: "block-1",
      mark_id: `${id}-mark`,
      orphaned: false,
      quote: "brown",
      version: 1,
    },
    ask_id: null,
    turn: null,
    author: { id: "bob", kind: "user" },
    body: "Suggested replacement.",
    created_at: "2026-09-10T00:00:00Z",
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: { accepted: null, replace_with: replaceWith },
    ...commentDeliveryFields(),
  });
  const first = suggestion("suggestion-1", "red");
  const second = suggestion("suggestion-2", "auburn");
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
  const actions: Array<[string, string]> = [];
  const selections: string[] = [];
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <KeymapProvider>
        <MarginSheet
          model={{
            actions: {
              closeComposer: () => {},
              onAction: (id, action) => {
                actions.push([id, action]);
              },
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
              onSelectCard: (key) => {
                selections.push(key);
              },
              openAskCount: 0,
              pendingActionIds: new Set<string>(),
              pinned: [],
              pinnedIds: [],
              resolvedThreads: [],
              retractedAskCount: 0,
              threads: [first, second].map((root) => ({
                anchor: root.anchor,
                key: root.id,
                lastReplyAt: undefined,
                replies: [],
                resolved: false,
                root: { comment: root, kind: "comment" },
              })),
              viewerLogin: "alice",
              visibleArtifact: specArtifact,
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
            sheet: {
              closeThread: () => {},
              expanded: true,
              threadKey: undefined,
              toggle: () => {},
            },
            filter: { blockId: "block-1", clear: () => {} },
            tab: { set: () => {}, value: "comments" },
          }}
        />
      </KeymapProvider>
    </QueryClientProvider>
  );

  try {
    expect(screen.queryByRole("dialog", { name: "Thread" })).toBeNull();
    const firstCard = screen.getByTestId(`margin-comment-${first.id}`);
    const secondCard = screen.getByTestId(`margin-comment-${second.id}`);
    expect(firstCard.querySelector("ins")?.textContent).toBe("red");
    expect(secondCard.querySelector("ins")?.textContent).toBe("auburn");
    fireEvent.click(within(secondCard).getByRole("button", { name: "Accept suggestion" }));
    expect(actions).toEqual([[second.id, "accept"]]);
    expect(selections).toEqual([]);
    expect(within(firstCard).getByRole("button", { name: "Reject suggestion" })).not.toBeNull();
  } finally {
    view.unmount();
    window.matchMedia = originalMatchMedia;
  }
});

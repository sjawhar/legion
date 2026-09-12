import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  fireEvent,
  type RenderResult,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { type ComponentProps, useState } from "react";

import { api } from "../../api/client";
import type { Comment } from "../../api/types";
import { ThreadCard } from "./ThreadCard";
import type { Thread } from "./useMarginItems";

function comment(
  id: string,
  body: string,
  createdAt: string,
  overrides: Partial<Comment> = {}
): Comment {
  return {
    anchor: {
      artifact_id: "artifact-1",
      block_id: null,
      mark_id: "mark-1",
      orphaned: false,
      quote: "selected text",
      version: 1,
    },
    ask_id: null,
    author: { id: "alice", kind: "user" },
    body,
    created_at: createdAt,
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    ...overrides,
  };
}

const root = comment("root-1", "Root comment", "2026-09-10T00:00:00Z");
const reply = comment("reply-1", "First reply", "2026-09-10T00:01:00Z", {
  anchor: null,
  reply_to: root.id,
});

function thread(overrides: Partial<Thread> = {}): Thread {
  return {
    anchor: root.anchor,
    key: root.id,
    lastReplyAt: reply.created_at,
    replies: [reply],
    resolved: false,
    root: { comment: root, kind: "comment" },
    ...overrides,
  };
}

function listThread(): Thread {
  const listRoot = comment("root-1", "- first\n- second", "2026-09-10T00:00:00Z");
  return thread({ replies: [], root: { comment: listRoot, kind: "comment" } });
}

type EditableCardProps = Omit<
  ComponentProps<typeof ThreadCard>,
  "editingCommentId" | "onEditingChange"
> &
  Partial<Pick<ComponentProps<typeof ThreadCard>, "editingCommentId" | "onEditingChange">>;

/** Owns the edit-in-progress id the way the margin does, so these tests drive the card alone. */
function EditableCard(props: EditableCardProps) {
  const [editingCommentId, setEditingCommentId] = useState<string>();
  return (
    <ThreadCard
      {...props}
      editingCommentId={props.editingCommentId ?? editingCommentId}
      onEditingChange={props.onEditingChange ?? setEditingCommentId}
    />
  );
}

function renderCard(
  currentThread = thread(),
  overrides: Partial<ComponentProps<typeof ThreadCard>> = {}
) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <EditableCard
        actionError={false}
        artifactSlug="spec"
        expanded
        hovered={false}
        isClosed={false}
        onAction={() => {}}
        onEdit={async () => undefined}
        onRetryAction={() => {}}
        onToggle={() => {}}
        owner={{ key: "CORE-1", kind: "issue" }}
        pendingAction={false}
        thread={currentThread}
        viewerLogin="alice"
        {...overrides}
      />
    </QueryClientProvider>
  );
}

test("an expanded comment thread renders flat replies and posts an inline reply to its root", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue({
    ...reply,
    body: "Second reply",
    id: "reply-2",
  });

  let view: RenderResult | undefined;
  try {
    view = renderCard();
    const card = screen.getByTestId(`margin-comment-${root.id}`);
    expect(card.getAttribute("aria-expanded")).toBe("true");
    expect(card.querySelector(`[data-margin-item="${reply.id}"]`)).toBeNull();
    expect((await screen.findByText("First reply")).closest("li")?.style.marginLeft).toBe("0px");

    const composer = screen.getByRole("form", { name: "Reply composer" });
    fireEvent.change(screen.getByLabelText("Reply"), { target: { value: "Second reply" } });
    fireEvent.keyDown(screen.getByLabelText("Reply"), { ctrlKey: true, key: "Enter" });

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "Second reply",
        reply_to: root.id,
      })
    );
    expect(composer.querySelector("blockquote")).toBeNull();
  } finally {
    view?.unmount();
    createComment.mockRestore();
  }
});

test("a collapsed preview flattens a list-only body onto one line with no nested <ul>", async () => {
  const view = renderCard(listThread(), { expanded: false });

  try {
    const preview = await within(view.container).findByText("first second");
    expect(preview.querySelector("ul")).toBeNull();
  } finally {
    view.unmount();
  }
});
test("a resolved comment thread names its resolver and exposes Reopen", () => {
  const onAction = spyOn({ call: () => {} }, "call");
  const resolvedRoot = comment("root-1", "Root comment", "2026-09-10T00:00:00Z", {
    resolved: true,
    resolved_at: "2026-09-10T00:02:00Z",
    resolved_by: { id: "bob", kind: "user" },
  });

  const view = renderCard(
    thread({ resolved: true, root: { comment: resolvedRoot, kind: "comment" } }),
    {
      onAction: onAction,
    }
  );
  try {
    expect(screen.getByText(/Resolved by bob/)).not.toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Reopen" }));
    expect(onAction).toHaveBeenCalledWith("root-1", "reopen");
  } finally {
    view.unmount();
    onAction.mockRestore();
  }
});

test("the author can edit a comment and an edited comment carries its marker", async () => {
  const onEdit = spyOn({ call: async () => undefined }, "call");
  const view = renderCard(undefined, { onEdit: onEdit });

  try {
    const rootEdit = within(screen.getByTestId(`margin-comment-${root.id}`)).getAllByRole(
      "button",
      { name: "Edit" }
    )[0];
    if (rootEdit === undefined) {
      throw new Error("The root comment has no Edit button.");
    }
    fireEvent.click(rootEdit);
    expect(screen.getByRole("button", { name: "Save" })).not.toBeNull();
    fireEvent.change(screen.getByLabelText("Edit comment"), { target: { value: "Updated root" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onEdit).toHaveBeenCalledWith(root.id, "Updated root"));

    view.rerender(
      <QueryClientProvider client={new QueryClient()}>
        <EditableCard
          actionError={false}
          artifactSlug="spec"
          expanded
          hovered={false}
          isClosed={false}
          onAction={() => {}}
          onEdit={async () => undefined}
          onRetryAction={() => {}}
          onToggle={() => {}}
          owner={{ key: "CORE-1", kind: "issue" }}
          pendingAction={false}
          thread={thread({
            root: {
              comment: { ...root, body: "Updated root", edited_at: "2026-09-10T00:02:00Z" },
              kind: "comment",
            },
          })}
          viewerLogin="alice"
        />
      </QueryClientProvider>
    );
    expect(await screen.findByText("Updated root")).not.toBeNull();
    expect(screen.getByTestId(`margin-comment-${root.id}`).textContent).toContain("edited");
  } finally {
    view.unmount();
    onEdit.mockRestore();
  }
});

test("a different author never sees an Edit control", () => {
  const view = renderCard(undefined, { viewerLogin: "bob" });
  try {
    expect(screen.queryByRole("button", { name: "Edit" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a comment body with inline code renders it as <code>", async () => {
  const codeRoot = comment("root-1", "Run `npm install` first.", "2026-09-10T00:00:00Z");
  const view = renderCard(thread({ root: { comment: codeRoot, kind: "comment" } }));
  try {
    expect(await screen.findByText("npm install", { selector: "code" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("Escape with an empty inline reply collapses the thread", () => {
  let collapsed = false;
  const view = renderCard(undefined, {
    onToggle: () => {
      collapsed = true;
    },
  });
  try {
    fireEvent.keyDown(screen.getByLabelText("Reply"), { key: "Escape" });
    expect(collapsed).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a terminal suggestion shows its disposition and has no Reopen control", () => {
  const accepted = comment("root-1", "Suggested replacement.", "2026-09-10T00:00:00Z", {
    resolved: true,
    resolved_at: "2026-09-10T00:02:00Z",
    resolved_by: { id: "bob", kind: "user" },
    suggestion: { accepted: true, replace_with: "Replacement" },
  });
  const view = renderCard(
    thread({
      resolved: true,
      root: { comment: accepted, kind: "comment" },
    })
  );
  try {
    expect(screen.queryByRole("button", { name: "Reopen" })).toBeNull();
    expect(screen.getByText(/Accepted by bob/)).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("a document-owned comment thread posts its inline reply to the artifact route", async () => {
  const createArtifactComment = spyOn(api, "createArtifactComment").mockResolvedValue(
    undefined as never
  );
  const documentRoot = comment("document-root", "Document-owned comment", "2026-09-10T00:00:00Z", {
    issue_key: null,
  });
  const view = renderCard(
    thread({
      key: documentRoot.id,
      root: { comment: documentRoot, kind: "comment" },
    }),
    {
      owner: {
        artifactId: "artifact-1",
        kind: "document",
        project: "CORE",
        slug: "design-notes",
      },
    }
  );
  try {
    const reply = screen.getByLabelText("Reply");
    fireEvent.change(reply, { target: { value: "Document reply" } });
    fireEvent.keyDown(reply, { ctrlKey: true, key: "Enter" });
    await waitFor(() =>
      expect(createArtifactComment).toHaveBeenCalledWith("artifact-1", {
        body: "Document reply",
        reply_to: documentRoot.id,
      })
    );
  } finally {
    view.unmount();
    createArtifactComment.mockRestore();
  }
});

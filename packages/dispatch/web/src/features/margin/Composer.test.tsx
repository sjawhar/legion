import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ApiError, api } from "../../api/client";
import { Composer, composerReferences } from "./Composer";

test("Composer uploads dropped files and inserts their references", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: {
      created_at: "2026-09-09T00:00:00Z",
      created_by: { id: "alice", kind: "user" },
      id: "artifact-1",
      issue_key: "CORE-1",
      kind: "file",
      name: "notes.md",
      primary: false,
      slug: "notes-md",
      versions: [],
    },
    version: {
      authors: [{ id: "alice", kind: "user" }],
      created_at: "2026-09-09T00:00:00Z",
      mime: "text/markdown",
      named: false,
      number: 1,
      sha256: "sha",
      size: 1,
      summary: null,
    },
  });

  try {
    const view = render(
      <QueryClientProvider client={queryClient}>
        <Composer
          anchor={{ artifact: "document-1", mark_id: "mark-1", quote: "text" }}
          owner={{ key: "CORE-1", kind: "issue" }}
          kind="comment"
          onClose={() => {}}
        />
      </QueryClientProvider>
    );

    const textarea = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.drop(textarea, {
      dataTransfer: {
        files: [new File(["# Notes"], "notes.md", { type: "text/markdown" })],
      },
    });

    await waitFor(() => expect(textarea.value).toBe("dispatch://CORE-1/artifact/notes-md"));
    view.unmount();
  } finally {
    uploadArtifact.mockRestore();
  }
});

test("Composer ignores a pasted project page URL because only documents are references", () => {
  const origin = "https://dispatch.test";

  expect(composerReferences(`${origin}/projects/CORE`, origin)).toEqual([]);
});

test("Composer preserves a project document reference with its document page href", () => {
  const origin = "https://dispatch.test";

  expect(composerReferences(`${origin}/projects/CORE/documents/design-notes`, origin)).toEqual([
    {
      href: "/projects/CORE/documents/design-notes",
      reference: "dispatch://CORE/artifact/design-notes",
    },
  ]);
  expect(composerReferences("dispatch://CORE/artifact/design-notes", origin)).toEqual([
    {
      href: "/projects/CORE/documents/design-notes",
      reference: "dispatch://CORE/artifact/design-notes",
    },
  ]);
});
test("Composer posts a document ask and comment to the artifact routes", async () => {
  const createArtifactAsk = spyOn(api, "createArtifactAsk").mockResolvedValue(undefined as never);
  const createArtifactComment = spyOn(api, "createArtifactComment").mockResolvedValue(
    undefined as never
  );
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const owner = {
    artifactId: "document-1",
    kind: "document" as const,
    project: "CORE",
    slug: "design-notes",
  };

  try {
    const ask = render(
      <QueryClientProvider client={queryClient}>
        <Composer
          anchor={{ artifact: "document-1", mark_id: "mark-1", quote: "selected" }}
          kind="ask"
          onClose={() => {}}
          owner={owner}
        />
      </QueryClientProvider>
    );
    fireEvent.change(screen.getByLabelText("Question"), { target: { value: "Why this text?" } });
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    await waitFor(() =>
      expect(createArtifactAsk).toHaveBeenCalledWith("document-1", {
        anchor: { artifact: "document-1", mark_id: "mark-1" },
        multiple: false,
        options: [],
        question: "Why this text?",
        urgency: "med",
      })
    );
    ask.unmount();

    const comment = render(
      <QueryClientProvider client={queryClient}>
        <Composer
          anchor={{ artifact: "document-1", mark_id: "mark-1", quote: "selected" }}
          kind="comment"
          onClose={() => {}}
          owner={owner}
        />
      </QueryClientProvider>
    );
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Please revise." } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    await waitFor(() =>
      expect(createArtifactComment).toHaveBeenCalledWith("document-1", {
        anchor: { artifact: "document-1", mark_id: "mark-1" },
        body: "Please revise.",
        reply_to: undefined,
      })
    );
    comment.unmount();
  } finally {
    createArtifactAsk.mockRestore();
    createArtifactComment.mockRestore();
  }
});

function renderComposer(kind: "ask" | "comment" | "suggestion") {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <Composer
        anchor={{ artifact: "document-1", mark_id: "mark-1", quote: "selected" }}
        owner={{ key: "CORE-1", kind: "issue" }}
        kind={kind}
        onClose={() => {}}
      />
    </QueryClientProvider>
  );
}

test("Composer sends the browser mark id for asks, comments, and suggestions", async () => {
  const createAsk = spyOn(api, "createAsk").mockResolvedValue(undefined as never);
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const anchor = { artifact: "document-1", mark_id: "mark-1", quote: "selected" };

  try {
    const ask = renderComposer("ask");
    fireEvent.change(screen.getByLabelText("Question"), { target: { value: "Why this text?" } });
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    await waitFor(() => expect(createAsk).toHaveBeenCalledTimes(1));
    const askPayload = createAsk.mock.calls[0]?.[1];
    expect(askPayload).toMatchObject({
      multiple: false,
      options: [],
      question: "Why this text?",
      urgency: "med",
    });
    expect(askPayload?.anchor).toEqual({ artifact: anchor.artifact, mark_id: anchor.mark_id });
    ask.unmount();

    const comment = renderComposer("comment");
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Please revise." } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const commentPayload = createComment.mock.calls[0]?.[1];
    expect(commentPayload).toMatchObject({ body: "Please revise." });
    expect(commentPayload?.anchor).toEqual({ artifact: anchor.artifact, mark_id: anchor.mark_id });
    comment.unmount();

    const suggestion = renderComposer("suggestion");
    fireEvent.change(screen.getByLabelText("Replacement"), { target: { value: "replacement" } });
    fireEvent.click(screen.getByRole("button", { name: "Suggest" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    const suggestionPayload = createComment.mock.calls[1]?.[1];
    expect(suggestionPayload).toMatchObject({
      body: "Suggested replacement.",
      suggestion: { replace_with: "replacement" },
    });
    expect(suggestionPayload?.anchor).toEqual({
      artifact: anchor.artifact,
      mark_id: anchor.mark_id,
    });
    suggestion.unmount();
  } finally {
    createAsk.mockRestore();
    createComment.mockRestore();
  }
});

test("Composer submits the configured ask options, multiple selection, and urgency", async () => {
  const createAsk = spyOn(api, "createAsk").mockResolvedValue(undefined as never);
  const anchor = { artifact: "document-1", mark_id: "mark-1", quote: "selected" };

  try {
    const composer = renderComposer("ask");
    fireEvent.change(screen.getByLabelText("Question"), { target: { value: "Which path?" } });
    fireEvent.click(screen.getByRole("button", { name: "High" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Allow multiple" }));
    const firstLabel = screen.getByLabelText("Option 1 label");
    fireEvent.change(firstLabel, { target: { value: "Ship" } });
    fireEvent.change(screen.getByLabelText("Option 1 description"), {
      target: { value: "Proceed now" },
    });
    fireEvent.keyDown(firstLabel, { key: "Enter" });
    await waitFor(() =>
      expect(document.activeElement).toBe(screen.getByLabelText("Option 2 label"))
    );
    fireEvent.change(screen.getByLabelText("Option 2 label"), { target: { value: "Hold" } });
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));

    await waitFor(() =>
      expect(createAsk).toHaveBeenCalledWith("CORE-1", {
        anchor: { artifact: anchor.artifact, mark_id: anchor.mark_id },
        multiple: true,
        options: [{ description: "Proceed now", label: "Ship" }, { label: "Hold" }],
        question: "Which path?",
        urgency: "high",
      })
    );
    composer.unmount();
  } finally {
    createAsk.mockRestore();
  }
});

test("Composer adds and removes ask option rows", () => {
  const composer = renderComposer("ask");

  try {
    expect(screen.getAllByLabelText(/Option \d+ label/)).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Add option" }));
    expect(screen.getAllByLabelText(/Option \d+ label/)).toHaveLength(2);
    fireEvent.click(screen.getByRole("button", { name: "Remove option 2" }));
    expect(screen.getAllByLabelText(/Option \d+ label/)).toHaveLength(1);
  } finally {
    composer.unmount();
  }
});

test("Escape after typing only an ask option prompts before discarding", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  let closed = false;
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Composer
        anchor={{ artifact: "document-1", mark_id: "mark-1", quote: "selected" }}
        owner={{ key: "CORE-1", kind: "issue" }}
        kind="ask"
        onClose={() => {
          closed = true;
        }}
      />
    </QueryClientProvider>
  );

  try {
    const label = screen.getByLabelText("Option 1 label");
    fireEvent.change(label, { target: { value: "Ship" } });
    fireEvent.keyDown(label, { key: "Escape" });

    await waitFor(() => expect(screen.getByText("Discard draft?")).toBeDefined());
    expect(closed).toBe(false);
  } finally {
    view.unmount();
  }
});

test("Composer shows the server's missing-anchor error and keeps the draft", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(409, { code: "ANCHOR_MISSING", error: "anchor mark is not in the document" })
  );

  try {
    const composer = renderComposer("comment");
    const comment = screen.getByLabelText<HTMLTextAreaElement>("Comment");
    fireEvent.change(comment, { target: { value: "Please revise." } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));

    await waitFor(() =>
      expect(screen.getByText("anchor mark is not in the document")).toBeTruthy()
    );
    expect(comment.value).toBe("Please revise.");
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    composer.unmount();
  } finally {
    createComment.mockRestore();
  }
});

test("Escape on a suggestion with only a replacement shows the discard prompt instead of closing silently", async () => {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  let closed = false;
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Composer
        anchor={{ artifact: "document-1", mark_id: "mark-1", quote: "selected" }}
        owner={{ key: "CORE-1", kind: "issue" }}
        kind="suggestion"
        onClose={() => {
          closed = true;
        }}
      />
    </QueryClientProvider>
  );

  try {
    fireEvent.change(screen.getByLabelText("Replacement"), { target: { value: "red" } });
    fireEvent.keyDown(screen.getByLabelText("Reason"), { key: "Escape" });

    await waitFor(() => expect(screen.getByText("Discard draft?")).toBeDefined());
    expect(closed).toBe(false);
  } finally {
    view.unmount();
  }
});

test("Escape from the composer while its reference picker opens closes only the picker", async () => {
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    artifacts: [],
    closed_at: null,
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: "CORE-1",
    labels: [],
    last_seq: 1,
    number: 1,
    parent: null,
    primary_artifact_id: "artifact-1",
    project: "CORE",
    route: null,
    status: "open",
    title: "Review the spec",
    updated_at: "2026-09-09T00:00:00Z",
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([]);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  let closed = false;
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Composer
        anchor={{ artifact: "document-1", mark_id: "mark-1", quote: "selected" }}
        owner={{ key: "CORE-1", kind: "issue" }}
        kind="comment"
        onClose={() => {
          closed = true;
        }}
      />
    </QueryClientProvider>
  );

  try {
    const comment = screen.getByLabelText("Comment");
    fireEvent.keyDown(comment, { ctrlKey: true, key: "k" });
    await screen.findByRole("dialog", { name: "Reference picker" });

    fireEvent.keyDown(comment, { key: "Escape" });

    expect(screen.queryByRole("dialog", { name: "Reference picker" })).toBeNull();
    expect(screen.getByRole("form", { name: "Comment composer" })).not.toBeNull();
    expect(closed).toBe(false);
  } finally {
    view.unmount();
    getIssue.mockRestore();
    listIssues.mockRestore();
  }
});
test("Enter in the reference picker's filter inserts the top match instead of submitting a ready-to-save draft", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const getIssue = spyOn(api, "getIssue").mockResolvedValue({
    artifacts: [],
    closed_at: null,
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    external_links: [],
    key: "CORE-1",
    labels: [],
    last_seq: 1,
    number: 1,
    parent: null,
    primary_artifact_id: "artifact-1",
    project: "CORE",
    route: null,
    status: "open",
    title: "Review the spec",
    updated_at: "2026-09-09T00:00:00Z",
  });
  const listIssues = spyOn(api, "listIssues").mockResolvedValue([
    {
      key: "CORE-2",
      open_asks: 0,
      parent: null,
      status: "todo",
      title: "Related work",
      updated_at: "2026-09-09T00:00:00Z",
    },
  ]);
  const view = renderComposer("comment");

  try {
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "why?" } });
    fireEvent.keyDown(screen.getByLabelText("Comment"), { ctrlKey: true, key: "k" });
    const filterInput = await screen.findByLabelText("Filter issues");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "CORE-2: Related work" })).toBeDefined()
    );

    fireEvent.keyDown(filterInput, { key: "Enter" });

    await waitFor(() =>
      expect((screen.getByLabelText("Comment") as HTMLTextAreaElement).value).toBe(
        "why? dispatch://CORE-2"
      )
    );
    expect(screen.queryByLabelText("Filter issues")).toBeNull();
    expect(createComment).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    createComment.mockRestore();
    getIssue.mockRestore();
    listIssues.mockRestore();
  }
});

test("Composer reports onSaved before onClose", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const calls: string[] = [];
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Composer
        anchor={{ artifact: "document-1", mark_id: "mark-1", quote: "selected" }}
        owner={{ key: "CORE-1", kind: "issue" }}
        kind="comment"
        onClose={() => calls.push("closed")}
        onSaved={() => calls.push("saved")}
      />
    </QueryClientProvider>
  );

  try {
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Please revise." } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    await waitFor(() => expect(calls).toEqual(["saved", "closed"]));
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("inline Composer posts a reply to its thread root with Ctrl+Enter", async () => {
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Composer
        inline
        kind="comment"
        onClose={() => {}}
        owner={{ key: "CORE-1", kind: "issue" }}
        replyTo="root-1"
      />
    </QueryClientProvider>
  );

  try {
    const reply = screen.getByLabelText("Reply");
    fireEvent.change(reply, { target: { value: "Inline reply" } });
    fireEvent.keyDown(reply, { ctrlKey: true, key: "Enter" });

    await waitFor(() =>
      expect(createComment).toHaveBeenCalledWith("CORE-1", {
        body: "Inline reply",
        reply_to: "root-1",
      })
    );
    expect(screen.getByRole("button", { name: "Reply" })).not.toBeNull();
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("edit Composer PATCHes the author comment body and reads Save", async () => {
  const editComment = spyOn(api, "editComment").mockResolvedValue(undefined as never);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Composer
        edit={{ body: "Original comment", id: "comment-1" }}
        owner={{ key: "CORE-1", kind: "issue" }}
        kind="comment"
        onClose={() => {}}
      />
    </QueryClientProvider>
  );

  try {
    expect(screen.getByRole("button", { name: "Save" })).not.toBeNull();
    const comment = screen.getByLabelText<HTMLTextAreaElement>("Edit comment");
    expect(comment.value).toBe("Original comment");
    fireEvent.change(comment, { target: { value: "Revised comment" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith("comment-1", { body: "Revised comment" })
    );
  } finally {
    view.unmount();
    editComment.mockRestore();
  }
});

test("edit Composer saves the text in the field at Save time, not the text of the last render", async () => {
  // A keystroke and the Save click can land in the same frame under load: the textarea already
  // holds the new text while React has not re-rendered with the new state. Saving the render's
  // text would PATCH the unedited body and mark it "edited".
  const editComment = spyOn(api, "editComment").mockResolvedValue(undefined as never);
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={queryClient}>
      <Composer
        edit={{ body: "Original comment", id: "comment-1" }}
        owner={{ key: "CORE-1", kind: "issue" }}
        kind="comment"
        onClose={() => {}}
      />
    </QueryClientProvider>
  );

  try {
    const comment = screen.getByLabelText<HTMLTextAreaElement>("Edit comment");
    // The browser has applied the keystroke; no change event has reached React yet.
    comment.value = "Edited comment";
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(editComment).toHaveBeenCalledWith("comment-1", { body: "Edited comment" })
    );
  } finally {
    view.unmount();
    editComment.mockRestore();
  }
});

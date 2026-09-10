import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ApiError, api } from "../../api/client";
import { Composer } from "./Composer";

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
          anchor={{ artifact: "document-1", quote: "text" }}
          issueKey="CORE-1"
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

function renderComposer(kind: "ask" | "comment" | "suggestion") {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <Composer
        anchor={{ artifact: "document-1", occurrence: 1, quote: "selected" }}
        issueKey="CORE-1"
        kind={kind}
        onClose={() => {}}
      />
    </QueryClientProvider>
  );
}

test("Composer sends only the selected quote occurrence for asks, comments, and suggestions", async () => {
  const createAsk = spyOn(api, "createAsk").mockResolvedValue(undefined as never);
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);
  const anchor = { artifact: "document-1", occurrence: 1, quote: "selected" };

  try {
    const ask = renderComposer("ask");
    fireEvent.change(screen.getByLabelText("Question"), { target: { value: "Why this text?" } });
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    await waitFor(() => expect(createAsk).toHaveBeenCalledTimes(1));
    const askPayload = createAsk.mock.calls[0]?.[1];
    expect(askPayload).toMatchObject({
      anchor,
      multiple: false,
      options: [],
      question: "Why this text?",
      urgency: "med",
    });
    expect(
      askPayload === undefined || askPayload.anchor === undefined
        ? true
        : "from" in askPayload.anchor
    ).toBe(false);
    ask.unmount();

    const comment = renderComposer("comment");
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Please revise." } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const commentPayload = createComment.mock.calls[0]?.[1];
    expect(commentPayload).toMatchObject({
      anchor,
      body: "Please revise.",
    });
    expect(
      commentPayload === undefined || commentPayload.anchor === undefined
        ? true
        : "from" in commentPayload.anchor
    ).toBe(false);
    comment.unmount();

    const suggestion = renderComposer("suggestion");
    fireEvent.change(screen.getByLabelText("Replacement"), { target: { value: "replacement" } });
    fireEvent.click(screen.getByRole("button", { name: "Suggest" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    const suggestionPayload = createComment.mock.calls[1]?.[1];
    expect(suggestionPayload).toMatchObject({
      anchor,
      body: "Suggested replacement.",
      suggestion: { replace_with: "replacement" },
    });
    expect(
      suggestionPayload === undefined || suggestionPayload.anchor === undefined
        ? true
        : "from" in suggestionPayload.anchor
    ).toBe(false);
    suggestion.unmount();
  } finally {
    createAsk.mockRestore();
    createComment.mockRestore();
  }
});

test("Composer submits the configured ask options, multiple selection, and urgency", async () => {
  const createAsk = spyOn(api, "createAsk").mockResolvedValue(undefined as never);
  const anchor = { artifact: "document-1", occurrence: 1, quote: "selected" };

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
        anchor,
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
        anchor={{ artifact: "document-1", occurrence: 1, quote: "selected" }}
        issueKey="CORE-1"
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

test("Composer shows the server's stale anchor error", async () => {
  const createComment = spyOn(api, "createComment").mockRejectedValue(
    new ApiError(409, { code: "ANCHOR_STALE", error: 'anchor quote "selected" is stale' })
  );

  try {
    const composer = renderComposer("comment");
    fireEvent.change(screen.getByLabelText("Comment"), { target: { value: "Please revise." } });
    fireEvent.click(screen.getByRole("button", { name: "Comment" }));

    await waitFor(() => expect(screen.getByText('anchor quote "selected" is stale')).toBeTruthy());
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
        anchor={{ artifact: "document-1", occurrence: 1, quote: "selected" }}
        issueKey="CORE-1"
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
        anchor={{ artifact: "document-1", occurrence: 1, quote: "selected" }}
        issueKey="CORE-1"
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

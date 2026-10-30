import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { type ReactNode, useEffect } from "react";
import { MemoryRouter } from "react-router-dom";

import { keyedAttempt } from "../../__tests__/keyed-attempt";
import { ApiError, api } from "../../api/client";
import type { ComposerAnchor, ComposerKind } from "../conversation/composer-model";
import { MentionComposer } from "../conversation/MentionComposer";
import type { RecordMarkPlacement } from "../doc/editor";
import { buildIssuePath, composerReferences } from "../refs/routes";
import { Margin } from "./Margin";
import { type DocumentBridge, MarginProvider, useMargin } from "./margin-context";
import { issue, specArtifact, stubDocumentBridge } from "./margin-fixture";
import type { MarginOwner } from "./useMarginItems";

function Composer({
  onSaved,
  owner,
  ...props
}: {
  anchor?: ComposerAnchor;
  autoFocus?: boolean;
  edit?: { body: string; id: string };
  inline?: boolean;
  kind: ComposerKind;
  onClose: () => void;
  onSaved?: () => void;
  owner: MarginOwner;
  replyTo?: string;
  saveEdit?: (id: string, body: string) => Promise<unknown>;
}) {
  return (
    <MentionComposer
      {...props}
      onSent={onSaved ?? (() => {})}
      owner={
        owner.kind === "issue"
          ? { issueKey: owner.key, kind: "issue" }
          : { artifactId: owner.artifactId, kind: "artifact", project: owner.project }
      }
      replyTo={
        props.replyTo === undefined
          ? null
          : {
              actor: { id: "alice", kind: "user" },
              excerpt: "",
              id: props.replyTo,
              parentKind: "comment",
            }
      }
    />
  );
}

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
      expect(createArtifactAsk).toHaveBeenCalledWith(
        "document-1",
        {
          anchor: { artifact: "document-1", mark_id: "mark-1" },
          multiple: false,
          options: [],
          question: "Why this text?",
          urgency: "med",
        },
        keyedAttempt
      )
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
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() =>
      expect(createArtifactComment).toHaveBeenCalledWith(
        "document-1",
        {
          anchor: { artifact: "document-1", mark_id: "mark-1" },
          body: "Please revise.",
          reply_to: undefined,
        },
        keyedAttempt
      )
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
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    const commentPayload = createComment.mock.calls[0]?.[1];
    expect(commentPayload).toMatchObject({ body: "Please revise." });
    expect(commentPayload?.anchor).toEqual({ artifact: anchor.artifact, mark_id: anchor.mark_id });
    comment.unmount();

    const suggestion = renderComposer("suggestion");
    fireEvent.change(screen.getByLabelText("Replacement"), { target: { value: "replacement" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
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
      expect(createAsk).toHaveBeenCalledWith(
        "CORE-1",
        {
          anchor: { artifact: anchor.artifact, mark_id: anchor.mark_id },
          multiple: true,
          options: [{ description: "Proceed now", label: "Ship" }, { label: "Hold" }],
          question: "Which path?",
          urgency: "high",
        },
        keyedAttempt
      )
    );
    composer.unmount();
  } finally {
    createAsk.mockRestore();
  }
});

// A send clears the whole draft, not the body alone. In a host that keeps the composer mounted,
// as this harness does, a sent suggestion's replacement or a sent ask's options left standing
// would read as a second draft waiting to go.
test("a sent suggestion or ask leaves no replacement or options behind", async () => {
  const createAsk = spyOn(api, "createAsk").mockResolvedValue(undefined as never);
  const createComment = spyOn(api, "createComment").mockResolvedValue(undefined as never);

  try {
    const suggestion = renderComposer("suggestion");
    const replacement = screen.getByLabelText<HTMLTextAreaElement>("Replacement");
    fireEvent.change(replacement, { target: { value: "replacement" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(replacement.value).toBe(""));
    suggestion.unmount();

    const ask = renderComposer("ask");
    fireEvent.change(screen.getByLabelText("Question"), { target: { value: "Which path?" } });
    fireEvent.change(screen.getByLabelText("Option 1 label"), { target: { value: "Ship" } });
    fireEvent.change(screen.getByLabelText("Option 1 description"), {
      target: { value: "Proceed now" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add option" }));
    fireEvent.change(screen.getByLabelText("Option 2 label"), { target: { value: "Hold" } });
    fireEvent.click(screen.getByRole("button", { name: "Ask" }));
    await waitFor(() => expect(createAsk).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.getAllByLabelText(/Option \d+ label/)).toHaveLength(1));
    expect(screen.getByLabelText<HTMLTextAreaElement>("Question").value).toBe("");
    expect(screen.getByLabelText<HTMLInputElement>("Option 1 label").value).toBe("");
    expect(screen.getByLabelText<HTMLInputElement>("Option 1 description").value).toBe("");
    ask.unmount();
  } finally {
    createAsk.mockRestore();
    createComment.mockRestore();
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

const MARK_ANCHOR = {
  artifact: specArtifact.id,
  mark_id: "m-1",
  // The words the editor read on each side of the selected ones, in the block it marked them in:
  // what it reported when the bar wrote the mark, and all the margin has to find them by once
  // the mark is gone.
  placement: { blockId: "b-1", prefix: "The quick brown fox ", readings: 1, suffix: "." },
  quote: "jumps over the lazy dog",
};

const ANCHOR_MISSING = new ApiError(409, {
  code: "ANCHOR_MISSING",
  error: "anchor mark is not in the document",
});

function RegisterDocument({ bridge }: { bridge: DocumentBridge }): ReactNode {
  const { registerDocument } = useMargin();
  useEffect(() => {
    registerDocument(bridge);
    return () => registerDocument(undefined);
  }, [bridge, registerDocument]);
  return null;
}

function ComposeOnMark({ kind }: { kind: ComposerKind }): ReactNode {
  const { composeForMark } = useMargin();
  return (
    <button
      onClick={() => {
        void composeForMark({ anchor: MARK_ANCHOR, kind }).catch(() => {});
      }}
      type="button"
    >
      Compose on the selection
    </button>
  );
}

/** The margin of an issue whose document `bridge` is, with the selection bar's compose on a
 *  press, as the document page renders it. */
function renderMarginCompose(bridge: DocumentBridge, kind: ComposerKind = "comment") {
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  queryClient.setQueryData(["issue", issue.key], issue);
  queryClient.setQueryData(["inbox"], []);
  queryClient.setQueryData(["asks", issue.key], []);
  queryClient.setQueryData(["user-state"], {});
  queryClient.setQueryData(["comments", issue.key], []);
  return render(
    <MemoryRouter initialEntries={[buildIssuePath({ key: issue.key, kind: "issue" })]}>
      <QueryClientProvider client={queryClient}>
        <MarginProvider>
          <RegisterDocument bridge={bridge} />
          <ComposeOnMark kind={kind} />
          <Margin />
        </MarginProvider>
      </QueryClientProvider>
    </MemoryRouter>
  );
}

/** Writes the draft `body` in the margin's composer and presses Send. */
async function sendMarginComment(body: string): Promise<HTMLElement> {
  fireEvent.click(screen.getByRole("button", { name: "Compose on the selection" }));
  const composer = await screen.findByRole("form", { name: "Comment composer" });
  fireEvent.change(within(composer).getByLabelText("Comment"), { target: { value: body } });
  fireEvent.click(within(composer).getByRole("button", { name: "Send" }));
  return composer;
}

/** Writes the draft `question` in the margin's ask composer and presses Ask. */
async function sendMarginAsk(question: string): Promise<HTMLElement> {
  fireEvent.click(screen.getByRole("button", { name: "Compose on the selection" }));
  const composer = await screen.findByRole("form", { name: "Comment composer" });
  fireEvent.change(within(composer).getByLabelText("Question"), { target: { value: question } });
  // The last "Ask" in the composer is its submit; the first is the kind switch's own button.
  fireEvent.click(within(composer).getAllByRole("button", { name: "Ask" }).at(-1) as HTMLElement);
  return composer;
}

test("the margin marks the words again and sends once more, under a key of its own, when the server refuses the anchor", async () => {
  const remarked: [string, string, string, RecordMarkPlacement | null][] = [];
  const bridge = stubDocumentBridge(specArtifact.id, {
    remarkMark(markId, kind, quote, placement) {
      remarked.push([markId, kind, quote, placement]);
      return { markId: "m-2", placement, quote: MARK_ANCHOR.quote };
    },
  });
  let attempts = 0;
  const createComment = spyOn(api, "createComment").mockImplementation(async () => {
    attempts += 1;
    if (attempts === 1) throw ANCHOR_MISSING;
    return undefined as never;
  });
  const view = renderMarginCompose(bridge);

  try {
    await sendMarginComment("Please revise.");
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    // The words the refused send itself carries reach the editor, and the reading of them the
    // composer kept with them: the editor may hold nothing else by the time another writer's
    // change has rebuilt the document.
    expect(remarked).toEqual([["m-1", "comment", MARK_ANCHOR.quote, MARK_ANCHOR.placement]]);
    expect(createComment.mock.calls[0]?.[1].anchor).toEqual({
      artifact: MARK_ANCHOR.artifact,
      mark_id: "m-1",
    });
    // The retry is a press of its own: the mark the margin wrote, under a key Dispatch has not
    // answered - a second body under the refused one would be refused again.
    expect(createComment.mock.calls[1]?.[1].anchor).toEqual({
      artifact: MARK_ANCHOR.artifact,
      mark_id: "m-2",
    });
    expect(createComment.mock.calls[1]?.[1].body).toBe("Please revise.");
    const keys = createComment.mock.calls.map((call) => call[2].key);
    expect(keys[0]).not.toBe(keys[1]);
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a second missing anchor after the margin re-anchored is the reader's to see, with no further retry", async () => {
  let minted = 1;
  const remarked: string[] = [];
  const bridge = stubDocumentBridge(specArtifact.id, {
    remarkMark(markId) {
      remarked.push(markId);
      minted += 1;
      return { markId: `m-${minted}`, placement: null, quote: MARK_ANCHOR.quote };
    },
  });
  const createComment = spyOn(api, "createComment").mockRejectedValue(ANCHOR_MISSING);
  const view = renderMarginCompose(bridge);

  try {
    const composer = await sendMarginComment("Please revise.");
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(within(composer).getByText("anchor mark is not in the document")).toBeTruthy()
    );
    expect(remarked).toEqual(["m-1"]);
    expect(createComment).toHaveBeenCalledTimes(2);
    expect(within(composer).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe(
      "Please revise."
    );
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

test("a refused re-anchor tells the reader the words are gone and keeps the draft", async () => {
  const bridge = stubDocumentBridge(specArtifact.id, {
    remarkMark: () => ({ refused: "gone" }),
  });
  const createComment = spyOn(api, "createComment").mockRejectedValue(ANCHOR_MISSING);
  const view = renderMarginCompose(bridge);

  try {
    const composer = await sendMarginComment("Please revise.");
    await waitFor(() =>
      expect(
        within(composer).getByText(
          "The text you selected was changed or deleted, so this comment has nothing to attach to. Your draft is kept — select the text again to attach it."
        )
      ).toBeTruthy()
    );
    expect(createComment).toHaveBeenCalledTimes(1);
    expect(within(composer).queryByText("anchor mark is not in the document")).toBeNull();
    expect(within(composer).getByLabelText<HTMLTextAreaElement>("Comment").value).toBe(
      "Please revise."
    );
    // Retry is still the reader's, and still the draft as it stands.
    fireEvent.click(within(composer).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(createComment).toHaveBeenCalledTimes(2));
  } finally {
    view.unmount();
    createComment.mockRestore();
  }
});

// What the reader is told is about what they are writing. An ask is no comment, and a refusal
// that calls it one tells them about something they never wrote (dispatch://INFRA-727).
test("a refused re-anchor on an ask calls it an ask, and never a comment", async () => {
  const bridge = stubDocumentBridge(specArtifact.id, {
    remarkMark: () => ({ refused: "gone" }),
  });
  const createAsk = spyOn(api, "createAsk").mockRejectedValue(ANCHOR_MISSING);
  const view = renderMarginCompose(bridge, "ask");

  try {
    const composer = await sendMarginAsk("Which dog?");
    await waitFor(() =>
      expect(
        within(composer).getByText(
          "The text you selected was changed or deleted, so this ask has nothing to attach to. Your draft is kept — select the text again to attach it."
        )
      ).toBeTruthy()
    );
    expect(composer.textContent).not.toContain("this comment");
    expect(within(composer).getByLabelText<HTMLTextAreaElement>("Question").value).toBe(
      "Which dog?"
    );
  } finally {
    view.unmount();
    createAsk.mockRestore();
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
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
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
      expect(createComment).toHaveBeenCalledWith(
        "CORE-1",
        {
          body: "Inline reply",
          reply_to: "root-1",
        },
        keyedAttempt
      )
    );
    expect(screen.getByRole("button", { name: "Send" })).not.toBeNull();
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
      expect(editComment).toHaveBeenCalledWith(
        "comment-1",
        { body: "Revised comment" },
        keyedAttempt
      )
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
      expect(editComment).toHaveBeenCalledWith(
        "comment-1",
        { body: "Edited comment" },
        keyedAttempt
      )
    );
  } finally {
    view.unmount();
    editComment.mockRestore();
  }
});

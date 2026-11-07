import { afterAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
// biome-ignore lint/style/noRestrictedImports: a paste and a drop of files, a form's own submit, the same-task pair of presses, a press on a control the send holds disabled, and a key that must not press the button it reaches
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../api/client";
import type {
  AnswerAskInput,
  Ask,
  AskEdit,
  AskRead,
  Comment,
  CreateCommentInput,
  InboxRow,
} from "../api/types";
import { AskCard } from "../features/inbox/AskCard";
import { AskReplyComposer } from "../features/inbox/AskThread";
import { Inbox } from "../features/inbox/Inbox";
import { commentDeliveryFields } from "./comment-fixture";
import { keyedAttempt } from "./keyed-attempt";

const getReferences = spyOn(api, "getReferences").mockResolvedValue({
  edges: [],
  node: { id: "ask-1", kind: "ask" },
});

afterAll(() => {
  getReferences.mockRestore();
});

function ask(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    author: { kind: "session", id: "session-1" },
    created_at: "2026-09-09T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    question: "Which option should ship?",
    state: "open",
    waiting_on: "human",
    urgency: "med",
    ...overrides,
  };
}

function answered(input: Ask, selected: string[], text: string | null = null): Ask {
  return {
    ...input,
    answer: { at: "2026-09-09T00:05:00Z", selected, text, user: "alice" },
    state: "answered",
  };
}

function reply(
  overrides: Partial<Omit<Comment, "resolved_by" | "resolved_at" | "edited_at">> = {}
): Comment {
  return {
    anchor: null,
    ask_id: "ask-1",
    turn: "agent",
    author: { kind: "user", id: "alice" },
    body: "A reply",
    created_at: "2026-09-09T00:01:00Z",
    id: "comment-1",
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_by: null,
    resolved_at: null,
    edited_at: null,
    suggestion: null,
    ...commentDeliveryFields(),
    ...overrides,
  };
}

// Every test provides an explicit (usually empty) thread so mounting AskCard
// never falls through to the default getAskThread, which would issue a real
// fetch in this test environment.
function emptyThread(input: Ask): () => Promise<AskRead> {
  return async () => ({
    ask: input,
    answers: [],
    claimant: null,
    edits: [],
    followers: [],
    replies: [],
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
  });
}

function renderCard(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  // The asker is an `AgentName`, whose live link needs a router.
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, view };
}

/** The element a forward Tab reaches from `from`: the next enabled, unhidden focusable element in
 *  document order (nothing these cards render sets a positive tabindex). */
function nextTabStop(from: Element): HTMLElement | undefined {
  const stops = Array.from(
    document.querySelectorAll<HTMLElement>("a[href], button, input, select, textarea, [tabindex]")
  ).filter(
    (element) =>
      element.tabIndex >= 0 && !element.matches(":disabled") && element.closest("[hidden]") === null
  );
  return stops[stops.indexOf(from as HTMLElement) + 1];
}

/** The element a control names in `aria-controls`. */
function controlledBy(control: HTMLElement): HTMLElement | null {
  const id = control.getAttribute("aria-controls");
  return id === null ? null : document.getElementById(id);
}

/** The reply bodies a thread shows, in the order it shows them. */
function shownReplyBodies(container: HTMLElement): (string | null | undefined)[] {
  return Array.from(
    container.querySelectorAll("li"),
    (item) => item.querySelector(".dispatch-markdown")?.textContent
  );
}

test("AskCard links a non-primary block ask to its owning artifact", async () => {
  const input = ask({
    block_artifact: { id: "artifact-design", primary: false, slug: "design-notes" },
    block_id: "decision-1",
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard ask={input} getAskThread={emptyThread(input)} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const link = await view.findByRole("link", { name: "Open in document" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1/artifacts/design-notes#b-decision-1");
  } finally {
    view.unmount();
  }
});

test("AskCard shows a quote anchor's document and quote", async () => {
  const input = {
    ...ask({
      anchor: {
        artifact_id: "artifact-design",
        block_id: "section-1",
        mark_id: "mark-1",
        orphaned: false,
        quote: "The chosen passage.",
        version: 1,
      },
    }),
    anchor_artifact: {
      name: "Design notes",
      primary: false,
      project: "CORE",
      slug: "design-notes",
    },
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard ask={input} getAskThread={emptyThread(input)} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const card = view.getByTestId(`ask-${input.id}`);
    const link = await within(card).findByRole("link", { name: "Design notes" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1/artifacts/design-notes#b-section-1");
    expect(within(card).getByText("The chosen passage.", { exact: true })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("AskCard keeps a deleted anchor document's quote", () => {
  const input = ask({
    anchor: {
      artifact_id: "deleted-artifact",
      block_id: "section-1",
      mark_id: "mark-1",
      orphaned: false,
      quote: "The surviving passage.",
      version: 1,
    },
  });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    expect(view.getByText("The surviving passage.", { exact: true })).toBeTruthy();
    // The deleted document's quote links nowhere; the only link left is the asker's live link.
    expect(view.getAllByRole("link").map((link) => link.getAttribute("href"))).toEqual([
      "/agents/session-1/live",
    ]);
  } finally {
    view.unmount();
  }
});

test("AskCard routes an unlinked quote anchor to its project document", async () => {
  const input = {
    ...ask({
      anchor: {
        artifact_id: "artifact-design",
        block_id: "section-1",
        mark_id: "mark-1",
        orphaned: false,
        quote: "The chosen passage.",
        version: 1,
      },
      issue_key: null,
    }),
    anchor_artifact: {
      name: "Design notes",
      primary: false,
      project: "CORE",
      slug: "design-notes",
    },
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard ask={input} getAskThread={emptyThread(input)} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const link = await view.findByRole("link", { name: "Design notes" });
    expect(link).toHaveAttribute("href", "/projects/CORE/documents/design-notes#b-section-1");
  } finally {
    view.unmount();
  }
});

test("AskCard omits a fragment for a legacy quote anchor without a block ID", async () => {
  const input = {
    ...ask(),
    anchor: {
      artifact_id: "artifact-design",
      mark_id: "mark-1",
      orphaned: false,
      quote: "The chosen passage.",
      version: 1,
    },
    anchor_artifact: {
      name: "Design notes",
      primary: true,
      project: "CORE",
      slug: "spec",
    },
  } as unknown as Ask;
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard ask={input} getAskThread={emptyThread(input)} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const link = await view.findByRole("link", { name: "Design notes" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1/spec");
  } finally {
    view.unmount();
  }
});

test("AskCard keeps an anchored document link after an answer", async () => {
  const input = answered(
    {
      ...ask({
        anchor: {
          artifact_id: "artifact-design",
          block_id: null,
          mark_id: "mark-1",
          orphaned: false,
          quote: "The chosen passage.",
          version: 1,
        },
      }),
      anchor_artifact: {
        name: "Design notes",
        primary: true,
        project: "CORE",
        slug: "spec",
      },
    },
    ["Ship"]
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard ask={input} getAskThread={emptyThread(input)} />
      </QueryClientProvider>
    </MemoryRouter>
  );
  try {
    const card = view.getByTestId(`ask-${input.id}`);
    const link = await within(card).findByRole("link", { name: "Design notes" });
    expect(link).toHaveAttribute("href", "/issues/CORE-1/spec");
  } finally {
    view.unmount();
  }
});

test("AskCard exposes medium urgency to screen readers without an inline label", () => {
  const input = ask({ urgency: "med" });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    expect(view.getByRole("article", { name: "Urgency: Medium" })).toBeTruthy();
    expect(view.queryByText("Medium", { exact: true })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("AskCard renders a blocking urgency notch", () => {
  const input = ask({ urgency: "blocking" });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    const card = view.getByRole("article", { name: "Urgency: Blocking" });
    expect(within(card).getByText("BLOCKING", { exact: true })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

// An untitled session is named by its short form, which sessions started in the same minute
// share; the asker's tooltip, the innermost title a browser shows, carries its full id.
test("an untitled session asker's tooltip carries its full id", () => {
  const input = ask({ author: { id: "0123456789abcdef-untitled", kind: "session" } });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    expect(view.getByTitle("session:01234567… · 0123456789abcdef-untitled")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("AskCard copies its tmux target and confirms success", async () => {
  const user = userEvent.setup();
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const input = ask({
    author: { id: "session-1", kind: "session", origin: { tmux: "dev:4.7" } },
  });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    // The session's own handles fold behind the author chip; open it first.
    await user.click(view.getByRole("button", { expanded: false, name: /session/ }));
    await user.click(view.getByRole("button", { name: "Copy tmux target dev:4.7" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("dev:4.7"));
    expect(await view.findByText("Copied", { exact: true })).toBeTruthy();
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    view.unmount();
  }
});

test("AskCard confirms tmux target copy through the legacy clipboard fallback", async () => {
  const user = userEvent.setup();
  const originalClipboard = navigator.clipboard;
  const originalExecCommand = document.execCommand;
  const execCommand = spyOn({ execCommand: (_command: string) => true }, "execCommand");
  Object.defineProperty(document, "execCommand", { configurable: true, value: execCommand });
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  expect(navigator.clipboard).toBeUndefined();
  const input = ask({
    author: { id: "session-1", kind: "session", origin: { tmux: "dev:4.7" } },
  });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    // The session's own handles fold behind the author chip; open it first.
    await user.click(view.getByRole("button", { expanded: false, name: /session/ }));
    await user.click(view.getByRole("button", { name: "Copy tmux target dev:4.7" }));
    expect(await view.findByText("Copied", { exact: true })).toBeTruthy();
  } finally {
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    view.unmount();
  }
});

test("AskCard shows an inline failure when neither clipboard path can copy", async () => {
  const user = userEvent.setup();
  const originalClipboard = navigator.clipboard;
  const originalExecCommand = document.execCommand;
  const execCommand = spyOn({ execCommand: (_command: string) => false }, "execCommand");
  Object.defineProperty(document, "execCommand", { configurable: true, value: execCommand });
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: undefined });
  expect(navigator.clipboard).toBeUndefined();
  const input = ask({
    author: { id: "session-1", kind: "session", origin: { tmux: "dev:4.7" } },
  });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    // The session's own handles fold behind the author chip; open it first.
    await user.click(view.getByRole("button", { expanded: false, name: /session/ }));
    await user.click(view.getByRole("button", { name: "Copy tmux target dev:4.7" }));
    expect(await view.findByText("Copy failed - select the text", { exact: true })).toBeTruthy();
    expect(view.getByText("dev:4.7", { exact: true })).toBeTruthy();
  } finally {
    Object.defineProperty(document, "execCommand", {
      configurable: true,
      value: originalExecCommand,
    });
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    view.unmount();
  }
});

test("AskCard copies its dispatch:// reference under its issue, else under the owning document", async () => {
  const user = userEvent.setup();
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const issueAsk = ask();
  const documentAsk = ask({ issue_key: null });
  const { view } = renderCard(
    <>
      <AskCard ask={issueAsk} getAskThread={emptyThread(issueAsk)} />
      <AskCard
        ask={documentAsk}
        getAskThread={emptyThread(documentAsk)}
        owner={{ project: "CORE", slug: "design-notes" }}
        variant="compact"
      />
    </>
  );

  try {
    await user.click(
      view.getByRole("button", { name: "Copy reference dispatch://CORE-1/ask/ask-1" })
    );
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("dispatch://CORE-1/ask/ask-1"));
    await user.click(
      view.getByRole("button", {
        name: "Copy reference dispatch://CORE/artifact/design-notes/ask/ask-1",
      })
    );
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith("dispatch://CORE/artifact/design-notes/ask/ask-1")
    );
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    view.unmount();
  }
});

// The same open anchored ask renders in more than one place at once (the issue board and
// the margin both show it) - each mounted AskCard's own answer field must stay independently
// labeled, not collide on an ask.id-derived id shared by every instance.
test("AskCard shows every previous version of an edited question, oldest first", async () => {
  const user = userEvent.setup();
  const editedAt = new Date().toISOString();
  const input = ask({
    edited_at: editedAt,
    options: [{ label: "SSE" }],
    question: "Should we use SSE?",
  });
  const edits: AskEdit[] = [
    {
      at: "2026-09-10T00:00:00Z",
      edited_by: { id: "session-1", kind: "session" },
      previous: {
        multiple: false,
        options: [{ label: "Email" }],
        question: "Should we use email?",
        urgency: "med",
      },
    },
    {
      at: "2026-09-10T01:00:00Z",
      edited_by: { id: "session-1", kind: "session" },
      previous: {
        multiple: false,
        options: [{ label: "Polling" }],
        question: "Should we use polling?",
        urgency: "med",
      },
    },
  ];
  const { view } = renderCard(
    <AskCard
      ask={input}
      getAskThread={async () => ({
        ask: input,
        answers: [],
        claimant: null,
        edits,
        followers: [],
        replies: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
      })}
    />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    await waitFor(() => expect(card).toHaveTextContent("Edited just now"));
    const summary = await view.findByText("Show 2 previous versions");
    const disclosure = summary.closest("details");
    if (!(disclosure instanceof HTMLDetailsElement)) {
      throw new Error("previous question disclosure is missing");
    }
    expect(disclosure.open).toBe(false);
    await user.click(summary);
    expect(disclosure.open).toBe(true);
    expect(await within(disclosure).findByText("Should we use email?")).toBeTruthy();
    expect(within(disclosure).getByText("Should we use polling?")).toBeTruthy();
    expect(within(disclosure).getAllByRole("list", { name: "Options" })[0]?.textContent).toContain(
      "Email"
    );
  } finally {
    view.unmount();
  }
});

test("two AskCard instances for the same ask keep independently labeled answer fields", () => {
  const input = ask();
  const { view } = renderCard(
    <>
      <AskCard ask={input} getAskThread={emptyThread(input)} />
      <AskCard ask={input} getAskThread={emptyThread(input)} />
    </>
  );

  try {
    const fields = view.getAllByLabelText("Your answer");
    expect(fields).toHaveLength(2);
    expect(fields[0]).not.toBe(fields[1]);
    expect(fields[0].id).not.toBe(fields[1].id);
  } finally {
    view.unmount();
  }
});

test("AskCard renders Markdown in the question, including bold text and a list", async () => {
  const input = ask({ question: "Ship **bold** or `code`?\n\n- item one\n- item two" });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    const card = view.getByTestId("ask-ask-1");
    expect(await within(card).findByText("bold", { selector: "strong" })).toBeTruthy();
    expect(
      within(card)
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual(["item one", "item two"]);
  } finally {
    view.unmount();
  }
});

test("AskCard keeps the backlink control its count earned after an answer", async () => {
  const user = userEvent.setup();
  const input = ask({ referenced_by_count: 2 });
  // The answer response is the one ask shape carrying no backlink count: it comes straight
  // back from the write, which never reads the graph. Answering moves no count either way.
  const { referenced_by_count: _dropped, ...withoutCount } = input;
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async () => answered(withoutCount, [], "Ship it.")}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    expect(view.getByRole("button", { name: "Referenced by (2)" })).toBeTruthy();
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("Ship it.");
    await user.click(view.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(view.getByText("Ship it.")).toBeTruthy());
    expect(view.getByRole("button", { name: "Referenced by (2)" })).toBeTruthy();

    // Opened, the panel carries no heading of its own: the control above it already reads
    // "Referenced by (2)", and a card that says it twice is a stutter.
    await user.click(view.getByRole("button", { name: "Referenced by (2)" }));
    const panel = await view.findByRole("region", { name: "Referenced by" });
    expect(within(panel).queryByRole("heading")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("AskCard submits the selected single option with the revision the human reviewed", async () => {
  const user = userEvent.setup();
  const submitted: Array<{
    id: string;
    input: { selected: string[]; text?: string; expected_edited_at: string | null };
  }> = [];
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (id, submission) => {
        submitted.push({ id, input: submission });
        return answered(input, submission.selected);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    const answer = view.getByLabelText("Your answer");
    expect(answer).toHaveAttribute(
      "placeholder",
      "Answer in your own words, or ask a question back"
    );
    const submit = view.getByRole("button", { name: "Answer" });
    expect(submit.hasAttribute("disabled")).toBe(true);

    await user.click(await view.findByRole("radio", { name: "Ship" }));
    expect(answer).toHaveAttribute("placeholder", "Add a note (optional)");
    expect(submit).not.toHaveAttribute("disabled");
    await user.click(submit);

    await waitFor(() =>
      expect(submitted).toEqual([
        { id: "ask-1", input: { selected: ["Ship"], expected_edited_at: null } },
      ])
    );
  } finally {
    view.unmount();
  }
});

/** A PNG as a clipboard or a drop hands it over. */
function png(name: string): File {
  return new File([new Uint8Array([137, 80, 78, 71])], name, { type: "image/png" });
}

// A picture pasted into an answer is part of the answer: the field takes the picture syntax once
// the upload lands, and Answer waits for it, since an answer sent before would go without it.
test("AskCard writes a picture pasted into its answer as an inline picture, and waits for it", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const { promise: landed, resolve: land } = Promise.withResolvers<void>();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockImplementation(async () => {
    await landed;
    return { artifact: { name: "shot.png", slug: "shot-png" }, version: { number: 1 } } as never;
  });
  const input = ask();
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    const answer = view.getByLabelText("Your answer") as HTMLTextAreaElement;
    await user.click(answer);
    await user.paste("This one");
    // user-event's paste carries text, never the file this row uploads.
    fireEvent.paste(answer, { clipboardData: { files: [png("shot.png")] } });
    await view.findByText("Uploading file…");
    expect(view.getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(true);
    // Ctrl+Enter is Answer's other way in, and the form's submit does not itself wait for uploads.
    await user.click(answer);
    await user.keyboard("{Control>}{Enter}{/Control}");
    const { promise: settled, resolve: settle } = Promise.withResolvers<void>();
    setTimeout(settle, 20);
    await settled;
    expect(submitted).toEqual([]);
    expect(uploadArtifact.mock.calls[0]?.[0]).toEqual({ issue: "CORE-1" });

    land();
    await waitFor(() =>
      expect(answer.value).toBe("This one ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)")
    );
    await user.click(view.getByRole("button", { name: "Answer" }));
    await waitFor(() =>
      expect(submitted).toEqual([
        {
          expected_edited_at: null,
          selected: [],
          text: "This one ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)",
        },
      ])
    );
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

// A document's ask belongs to no issue: a picture in its reply is the document's project's.
test("AskCard uploads a picture dropped into a document ask's reply to the document's project", async () => {
  const uploadArtifact = spyOn(api, "uploadArtifact").mockResolvedValue({
    artifact: { name: "shot.png", slug: "shot-png" },
    version: { number: 2 },
  } as never);
  const input = answered(ask({ artifact_id: "artifact-design", issue_key: null }), [], "Yes");
  const { view } = renderCard(
    <AskCard
      ask={input}
      getAskThread={emptyThread(input)}
      owner={{ project: "CORE", slug: "design-notes" }}
    />
  );

  try {
    const reply = (await view.findByLabelText("Reply")) as HTMLTextAreaElement;
    // user-event's drop carries no files, and the upload is what this row is about.
    fireEvent.drop(reply, { dataTransfer: { files: [png("shot.png")] } });
    await waitFor(() =>
      expect(reply.value).toBe("![shot.png](dispatch://CORE/artifact/shot-png@v2)")
    );
    expect(uploadArtifact.mock.calls[0]?.[0]).toEqual({ project: "CORE" });
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

test("AskCard submits every checked multiple option", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({ multiple: true, options: [{ label: "Docs" }, { label: "Tests" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("checkbox", { name: "Docs" }));
    await user.click(view.getByRole("checkbox", { name: "Tests" }));
    await user.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() =>
      expect(submitted).toEqual([{ selected: ["Docs", "Tests"], expected_edited_at: null }])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard submits free-text without inventing a selected option", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask();
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("Take the third path");
    await user.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() =>
      expect(submitted).toEqual([
        { selected: [], text: "Take the third path", expected_edited_at: null },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard submits on Ctrl+Enter but not on plain Enter", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask();
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    const field = view.getByLabelText("Your answer");
    await user.click(field);
    await user.paste("Take the third path");
    await user.keyboard("{Enter}");
    expect(submitted).toEqual([]);
    await user.keyboard("{Control>}{Enter}{/Control}");

    await waitFor(() =>
      expect(submitted).toEqual([
        { selected: [], text: "Take the third path", expected_edited_at: null },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard submits a selected Other option with free text", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    const submit = view.getByRole("button", { name: "Answer" });
    const other = await view.findByRole("radio", { name: "Other" });
    await user.click(other);
    expect(submit.hasAttribute("disabled")).toBe(true);
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("Try a hybrid");
    expect(submit).not.toHaveAttribute("disabled");
    await user.click(submit);

    await waitFor(() =>
      expect(submitted).toEqual([{ selected: [], text: "Try a hybrid", expected_edited_at: null }])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard offers to ask back for a question-shaped answer without closing the ask", async () => {
  const user = userEvent.setup();
  const answerCalls: AnswerAskInput[] = [];
  const replyCalls: Array<{ issueKey: string; input: CreateCommentInput }> = [];
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        answerCalls.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      createReply={async (issueKey, replyInput) => {
        replyCalls.push({ issueKey, input: replyInput });
        return reply({ ask_id: replyInput.ask_id, body: replyInput.body });
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("How does this fit our release plan?");
    await user.click(view.getByRole("button", { name: "Answer" }));

    const choice = await view.findByRole("group", { name: "Question-shaped answer" });
    const clarification = within(choice).getByRole("button", { name: "Ask back instead" });
    expect(answerCalls).toEqual([]);
    expect(clarification).toHaveFocus();
    // Enter on the focused button is the press: user-event's keyboard activates it, so the
    // separate click this row used to make would ask back a second time.
    await user.keyboard("{Enter}");

    await waitFor(() =>
      expect(replyCalls).toEqual([
        {
          issueKey: "CORE-1",
          input: { ask_id: "ask-1", body: "How does this fit our release plan?" },
        },
      ])
    );
    expect(answerCalls).toEqual([]);
    expect(view.getByTestId("ask-ask-1")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("AskCard lets a human explicitly answer with a question-shaped response", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("Why wait?");
    await user.click(view.getByRole("button", { name: "Answer" }));
    await user.click(await view.findByRole("button", { name: "Answer with it anyway" }));

    await waitFor(() =>
      expect(submitted).toEqual([{ selected: [], text: "Why wait?", expected_edited_at: null }])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard dismisses the question-shaped guard when the response becomes an answer", async () => {
  const user = userEvent.setup();
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    const field = view.getByLabelText("Your answer");
    await user.click(field);
    await user.paste("Why wait?");
    await user.click(view.getByRole("button", { name: "Answer" }));
    await view.findByRole("button", { name: "Ask back instead" });

    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Ship it.");
    expect(view.queryByRole("button", { name: "Ask back instead" })).not.toBeInTheDocument();
    expect(view.getByRole("button", { name: "Answer" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("an approval-kind ask labels itself Approval requested and has one reason field", async () => {
  const input = ask({
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve this document?",
  });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    const card = view.getByTestId("ask-ask-1");
    expect(within(card).getByText("Approval requested")).toBeTruthy();
    await view.findByRole("radio", { name: "Approve" });
    expect(within(card).getByRole("radio", { name: "Request changes" })).toBeTruthy();
    expect(within(card).queryByRole("radio", { name: "Other" })).not.toBeInTheDocument();
    expect(view.getByLabelText("Reason")).toHaveAttribute(
      "placeholder",
      "Answer in your own words, or ask a question back"
    );
  } finally {
    view.unmount();
  }
});

test("an approval-kind ask submits Approve with an optional note", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
  });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("radio", { name: "Approve" }));
    const reason = view.getByLabelText("Reason");
    expect(reason).toHaveAttribute("placeholder", "Note (optional)");
    await user.click(reason);
    await user.paste("Ready to ship.");
    await user.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() =>
      expect(submitted).toEqual([
        { selected: ["Approve"], text: "Ready to ship.", expected_edited_at: null },
      ])
    );
  } finally {
    view.unmount();
  }
});

// A refusal of what the answer says, or of the ask it answers, is refused again however often it
// is sent, so the card says why and offers no Retry; anything else may pass on a retry. A
// CAP_EXCEEDED answer means even its server state would leave the document past what the server
// can decode, and the card shows the server's reason.
for (const refusal of [
  {
    code: "ASK_CLOSED",
    error: "ask is already answered",
    message: "This ask was already answered",
    status: 409,
  },
  {
    code: "ASK_RESOLVED",
    error: "ask is already resolved",
    message: "This ask was closed",
    status: 409,
  },
  { code: "ASK_EDITED", error: "question changed", message: "the question changed", status: 409 },
  {
    code: "CAP_EXCEEDED",
    error:
      "document too large to store: this change would leave the document's live state past the 1048576 items ygo decodes in one update; shorten the change, or split the document",
    message: "past the 1048576 items ygo decodes",
    status: 413,
  },
]) {
  test(`an answer refused with ${refusal.code} says so and offers no Retry`, async () => {
    const user = userEvent.setup();
    let calls = 0;
    const input = ask({ options: [{ label: "Ship" }] });
    const { view } = renderCard(
      <AskCard
        ask={input}
        answerAsk={async () => {
          calls++;
          throw new ApiError(refusal.status, { code: refusal.code, error: refusal.error });
        }}
        getAskThread={emptyThread(input)}
      />
    );

    try {
      await user.click(await view.findByRole("radio", { name: "Ship" }));
      await user.click(view.getByRole("button", { name: "Answer" }));

      const alert = await view.findByRole("alert");
      expect(alert).toHaveTextContent(refusal.message);
      expect(within(alert).queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
      expect(calls).toBe(1);
    } finally {
      view.unmount();
    }
  });
}

test("an answer that failed for another reason offers a Retry that sends it again", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({ options: [{ label: "Ship" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        if (submitted.length === 1) {
          throw new Error("offline");
        }
        return answered(input, submission.selected);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("radio", { name: "Ship" }));
    await user.click(view.getByRole("button", { name: "Answer" }));

    const alert = await view.findByRole("alert");
    expect(alert).toHaveTextContent("Could not save your answer.");
    await user.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(submitted).toHaveLength(2));
    expect(submitted[1]).toEqual(submitted[0]);
  } finally {
    view.unmount();
  }
});

test("an approval-kind ask names the reason Request changes needs, focuses its field, and says why Answer is dead", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
  });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    expect(view.queryByText("Add a reason to send Request changes")).not.toBeInTheDocument();
    await user.click(await view.findByRole("radio", { name: "Request changes" }));
    const reasonField = view.getByLabelText("Reason (required)");
    expect(reasonField).toHaveFocus();
    const submit = view.getByRole("button", { name: "Answer" });
    expect(submit.hasAttribute("disabled")).toBe(true);
    expect(submit).toHaveAttribute("title", "Add a reason to send Request changes");
    expect(view.getByText("Add a reason to send Request changes")).toBeTruthy();

    await user.click(reasonField);
    await user.paste("Needs another pass");
    expect(submit).not.toHaveAttribute("disabled");
    expect(submit).not.toHaveAttribute("title");
    expect(view.queryByText("Add a reason to send Request changes")).not.toBeInTheDocument();
    await user.click(submit);

    await waitFor(() =>
      expect(submitted).toEqual([
        { selected: ["Request changes"], text: "Needs another pass", expected_edited_at: null },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("a Done / Can't question is an ordinary question: Other stays, Can't needs no reason", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({
    options: [{ label: "Done" }, { label: "Can't" }],
    question: "Confirm the deployment.",
  });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    expect(within(card).queryByText("Action", { exact: true })).not.toBeInTheDocument();
    await user.click(await within(card).findByRole("radio", { name: "Can't" }));
    expect(within(card).getByRole("radio", { name: "Other" })).toBeTruthy();
    expect(within(card).queryByLabelText("Reason (required)")).not.toBeInTheDocument();
    const answer = within(card).getByRole("button", { name: "Answer" });
    expect(answer).not.toHaveAttribute("disabled");
    expect(answer).not.toHaveAttribute("title");
    await user.click(answer);
    await waitFor(() =>
      expect(submitted).toEqual([{ selected: ["Can't"], expected_edited_at: null }])
    );
  } finally {
    view.unmount();
  }
});

test("the compact card opens and focuses the reason field when Request changes is picked, again when the disclosure reopens", async () => {
  const user = userEvent.setup();
  const input = ask({
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
  });
  const { view } = renderCard(
    <AskCard ask={input} getAskThread={emptyThread(input)} variant="compact" />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    await user.click(await within(card).findByRole("radio", { name: "Approve" }));
    expect(within(card).queryByLabelText(/Reason|Your answer/)).not.toBeInTheDocument();
    expect(within(card).getByRole("button", { name: "Answer" })).not.toHaveAttribute("disabled");

    await user.click(within(card).getByRole("radio", { name: "Request changes" }));
    const reason = within(card).getByLabelText("Reason (required)");
    expect(reason).toHaveFocus();
    const answer = within(card).getByRole("button", { name: "Answer" });
    expect(answer.hasAttribute("disabled")).toBe(true);
    expect(within(card).getByText("Add a reason to send Request changes")).toBeTruthy();
    await user.click(reason);
    await user.paste("Needs another pass.");
    expect(answer).not.toHaveAttribute("disabled");

    // Closing the disclosure unmounts the field; Request changes is still the picked radio, so
    // reopening the disclosure is what brings the field back, and it must take focus again.
    await user.click(
      within(card).getByRole("button", { name: "Add a note or answer in your own words" })
    );
    expect(within(card).queryByLabelText("Reason (required)")).not.toBeInTheDocument();
    await user.click(
      within(card).getByRole("button", { name: "Add a note or answer in your own words" })
    );
    const reopened = within(card).getByLabelText("Reason (required)");
    expect(reopened).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("AskCard submits selected options alongside a typed note", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({ multiple: true, options: [{ label: "Docs" }, { label: "Tests" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("checkbox", { name: "Docs" }));
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("Also update the wiki");
    await user.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() =>
      expect(submitted).toEqual([
        { selected: ["Docs"], text: "Also update the wiki", expected_edited_at: null },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard requires text when Other is selected with real multiple options", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({ multiple: true, options: [{ label: "Docs" }, { label: "Tests" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("checkbox", { name: "Docs" }));
    await user.click(view.getByRole("checkbox", { name: "Other" }));
    const answer = view.getByRole("button", { name: "Answer" });
    expect(answer.hasAttribute("disabled")).toBe(true);
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("Also update the guide.");
    expect(answer).not.toHaveAttribute("disabled");
    await user.click(answer);

    await waitFor(() =>
      expect(submitted).toEqual([
        {
          selected: ["Docs"],
          text: "Also update the guide.",
          expected_edited_at: null,
        },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard submits a selected single option with added context", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        return answered(input, submission.selected, submission.text ?? null);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("radio", { name: "Ship" }));
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("After review.");
    await user.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() =>
      expect(submitted).toEqual([
        { selected: ["Ship"], text: "After review.", expected_edited_at: null },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard restores its inbox entry if an optimistic answer fails", async () => {
  const user = userEvent.setup();
  let rejectAnswer: (reason?: unknown) => void = () => {};
  const pendingAnswer = new Promise<Ask>((_resolve, reject) => {
    rejectAnswer = reject;
  });
  const input = ask({ options: [{ label: "Ship" }] });
  const { queryClient, view } = renderCard(
    <AskCard ask={input} answerAsk={() => pendingAnswer} getAskThread={emptyThread(input)} />
  );
  const inboxInput: InboxRow = {
    ...input,
    priority: null,
    snoozed_until: null,
    thread: {
      answers: [],
      claimant: null,
      edits: [],
      followers: [],
      replies: [],
      route: null,
      route_status: null,
      route_holder: null,
      route_status_reason: null,
    },
  };
  queryClient.setQueryData<InboxRow[]>(["inbox"], [inboxInput]);

  try {
    await user.click(await view.findByRole("radio", { name: "Ship" }));
    await user.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(queryClient.getQueryData<InboxRow[]>(["inbox"])).toEqual([]));
    rejectAnswer(new Error("offline"));
    await waitFor(() =>
      expect(queryClient.getQueryData<InboxRow[]>(["inbox"])).toEqual([inboxInput])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard reloads a changed question, requires the human to reconfirm, and answers the new wording", async () => {
  const user = userEvent.setup();
  const submitted: AnswerAskInput[] = [];
  const input = ask({ options: [{ label: "Ship" }] });
  const current = ask({
    edited_at: "2026-09-12T12:00:00Z",
    options: [{ label: "Hold" }],
    question: "Should we hold this release?",
  });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => {
        submitted.push(submission);
        if (submitted.length === 1) {
          throw new ApiError(409, { code: "ASK_EDITED", error: "question changed" });
        }
        return answered(current, submission.selected);
      }}
      getAskThread={async () => ({
        ask: current,
        answers: [],
        edits: [],
        claimant: null,
        followers: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
        replies: [],
      })}
    />
  );

  try {
    await user.click(await view.findByRole("radio", { name: "Ship" }));
    await user.click(view.getByRole("button", { name: "Answer" }));

    await view.findByText(/This question changed while you were answering/);
    expect(view.getByText("Should we hold this release?")).toBeTruthy();
    expect(view.getByRole("radio", { name: "Hold" })).not.toHaveAttribute("checked");
    expect(view.getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(true);
    expect(
      within(view.getByRole("alert")).queryByRole("button", { name: "Retry" })
    ).not.toBeInTheDocument();

    await user.click(view.getByRole("radio", { name: "Hold" }));
    await user.click(view.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(submitted).toHaveLength(2));
    expect(submitted[1]).toMatchObject({
      selected: ["Hold"],
      expected_edited_at: "2026-09-12T12:00:00Z",
    });
  } finally {
    view.unmount();
  }
});

test("an answered ask lists every option, marks the selection, and shows when it was asked and answered", async () => {
  const input = ask({
    created_at: "2026-09-10T09:00:00Z",
    options: [{ label: "Ship" }, { description: "Wait for QA", label: "Hold" }],
  });
  const { view } = renderCard(
    <AskCard ask={answered(input, ["Ship"])} getAskThread={emptyThread(input)} />
  );

  try {
    const options = within(view.getByRole("list", { name: "Options" })).getAllByRole("listitem");
    expect(options.map((option) => option.dataset.selected)).toEqual(["true", "false"]);
    expect(within(options[0] as HTMLElement).getByLabelText("Selected")).toBeTruthy();
    await waitFor(() => expect(options[1]?.textContent).toContain("Wait for QA"));

    const card = view.getByTestId("ask-ask-1");
    expect(card.textContent).toMatch(/Asked .* · Answered by alice/);
    expect(card.querySelectorAll("time")).toHaveLength(2);
    expect(card).toHaveTextContent("session:session-…");
  } finally {
    view.unmount();
  }
});

test("AskCard shows submitted free text in its answered card", async () => {
  const user = userEvent.setup();
  const input = ask({
    options: [
      { description: "Ship immediately", label: "Ship" },
      { description: "Wait for review", label: "Hold" },
    ],
  });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async () => answered(input, ["Ship"], "Proceed.")}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("radio", { name: /^Ship/ }));
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("Proceed.");
    await user.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
    const card = view.getByTestId("ask-ask-1");
    expect(card.textContent).toMatch(/Asked .* · Answered by alice/);
    expect(card.querySelectorAll("time")).toHaveLength(2);
    expect(within(card).getAllByRole("listitem")[0]?.dataset.selected).toBe("true");
    expect(view.getByText("Proceed.")).toBeTruthy();
    expect(view.queryByRole("button", { name: "Answer" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("an answered ask records a chosen Other answer under an Other label, not as an addendum", async () => {
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard
      ask={answered(input, [], "Neither, let's wait a week")}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    expect(await within(card).findByText("Other")).toBeTruthy();
    expect(within(card).getByText("Neither, let's wait a week")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("AskCard retries a failed answer without losing its form", async () => {
  const user = userEvent.setup();
  let attempts = 0;
  const input = ask({ options: [{ label: "Ship" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async () => {
        attempts += 1;
        if (attempts === 1) {
          throw new Error("offline");
        }
        return answered(input, ["Ship"]);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("radio", { name: "Ship" }));
    await user.click(view.getByRole("button", { name: "Answer" }));
    await view.findByRole("alert");
    await user.click(view.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
    expect(attempts).toBe(2);
  } finally {
    view.unmount();
  }
});

test("AskCard ignores a same-task duplicate answer submit", async () => {
  const user = userEvent.setup();
  let attempts = 0;
  const input = ask({ options: [{ label: "Ship" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async () => {
        attempts += 1;
        return answered(input, ["Ship"]);
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("radio", { name: "Ship" }));
    const submit = view.getByRole("button", { name: "Answer" });
    // Both presses land in one task, before the render that disables Answer under them: that is
    // the duplicate this row ignores, and user-event renders between its own interactions.
    fireEvent.click(submit);
    fireEvent.click(submit);

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
    expect(attempts).toBe(1);
  } finally {
    view.unmount();
  }
});

test("AskCard renders replies under the question without an open-ask thread composer", async () => {
  const input = ask();
  const { view } = renderCard(
    <AskCard
      ask={input}
      getAskThread={async () => ({
        ask: input,
        answers: [],
        edits: [],
        claimant: null,
        followers: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
        replies: [reply({ id: "comment-1", body: "Any update?" })],
      })}
    />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    await waitFor(() => expect(card).toHaveTextContent("Any update?"));
    expect(
      within(view.getByTestId("thread-ask-1")).getByText("alice", { exact: false })
    ).toBeTruthy();
    expect(view.queryByLabelText("Reply")).not.toBeInTheDocument();
    expect(view.queryByText("Replying does not answer the question.")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("AskCard keeps the thread and reply composer visible after the ask is answered", async () => {
  const user = userEvent.setup();
  const input = ask({ options: [{ label: "Ship" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async () => answered(input, ["Ship"])}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(await view.findByRole("radio", { name: "Ship" }));
    await user.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
    // Answering is a distinct event, not the end of the conversation: the
    // thread section (and its composer) stays mounted below the answer.
    expect(view.getByRole("button", { name: "Reply" })).toBeTruthy();
    expect(view.getByLabelText("Reply")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("the thread composer only appears after an ask is answered", async () => {
  const openAsk = ask();
  const { view: openView } = renderCard(
    <AskCard ask={openAsk} getAskThread={emptyThread(openAsk)} />
  );
  const answeredInput = ask({ options: [{ label: "Ship" }] });
  const answeredAsk = answered(answeredInput, ["Ship"]);
  const { view: answeredView } = renderCard(
    <AskCard ask={answeredAsk} getAskThread={emptyThread(answeredAsk)} />
  );

  try {
    const openScope = within(openView.container);
    const answeredScope = within(answeredView.container);
    expect(await openScope.findByLabelText("Your answer")).toBeTruthy();
    expect(openScope.queryByLabelText("Reply")).not.toBeInTheDocument();

    expect(await answeredScope.findByLabelText("Reply")).toBeTruthy();
    expect(answeredScope.getByRole("button", { name: "Reply" })).toBeTruthy();
    expect(
      answeredScope.queryByText("Replying does not answer the question.")
    ).not.toBeInTheDocument();
  } finally {
    openView.unmount();
    answeredView.unmount();
  }
});

test("AskCard asks back from its answer field and clears the text without clearing the selection", async () => {
  const user = userEvent.setup();
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const posted: Array<{ issueKey: string; body: string; askId?: string }> = [];
  const { view } = renderCard(
    <AskCard
      ask={input}
      createReply={async (issueKey, body) => {
        posted.push({ issueKey, body: body.body, askId: body.ask_id });
        return reply({ body: body.body });
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    const ship = await view.findByRole("radio", { name: "Ship" });
    await user.click(ship);
    const field = view.getByLabelText("Your answer");
    await user.click(field);
    await user.paste("Any update on this?");
    await user.click(view.getByRole("button", { name: "Ask back" }));

    await waitFor(() =>
      expect(posted).toEqual([{ issueKey: "CORE-1", body: "Any update on this?", askId: "ask-1" }])
    );
    await waitFor(() => expect(field).toHaveProperty("value", ""));
    expect((ship as HTMLInputElement).checked).toBe(true);
  } finally {
    view.unmount();
  }
});

test("AskCard surfaces a retryable error when asking back fails", async () => {
  const user = userEvent.setup();
  let attempts = 0;
  const input = ask();
  const { view } = renderCard(
    <AskCard
      ask={input}
      createReply={async (_issueKey, body) => {
        attempts += 1;
        if (attempts === 1) {
          throw new Error("offline");
        }
        return reply({ body: body.body });
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    await user.click(view.getByLabelText("Your answer"));
    await user.paste("Retry me");
    await user.click(view.getByRole("button", { name: "Ask back" }));
    const alert = await view.findByRole("alert");
    expect(alert).toHaveTextContent("Could not send your clarification.");
    await user.click(view.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(attempts).toBe(2));
  } finally {
    view.unmount();
  }
});

test("a resolved ask keeps its question and options and carries a resolution badge", async () => {
  const input = {
    ...ask({
      options: [{ label: "Ship" }, { label: "Hold" }],
    }),
    resolution: {
      actor: { id: "session-1", kind: "session" },
      at: "2026-09-10T09:30:00Z",
      kind: "retracted",
      reason: "Superseded.",
    },
    state: "resolved",
  } as unknown as Ask;
  // The resolution names its actor with a link to that agent's live conversation; `renderCard`
  // renders inside a router, as the app always does.
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    expect(await view.findByText(input.question)).toBeTruthy();
    expect(view.getByRole("list", { name: "Options" }).children).toHaveLength(2);
    expect(view.getByTestId("ask-resolution-badge").textContent).toBe("Retracted");
    const resolution = input.resolution;
    if (resolution === undefined) {
      throw new Error("resolved Ask fixture is missing its resolution");
    }
    await waitFor(() => expect(view.getAllByText(resolution.reason)).toHaveLength(1));
    expect(view.queryByRole("button", { name: "Reply" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("AskCard shows the newest two replies, expands the earlier ones, and puts a fresh reply last", async () => {
  const user = userEvent.setup();
  const input = answered(ask(), ["Ship"]);
  let replies = [
    reply({ body: "Oldest reply", created_at: "2026-09-09T00:01:00Z", id: "comment-1" }),
    reply({ body: "Older reply", created_at: "2026-09-09T00:02:00Z", id: "comment-2" }),
    reply({ body: "Middle reply", created_at: "2026-09-09T00:03:00Z", id: "comment-3" }),
    reply({ body: "Newer reply", created_at: "2026-09-09T00:04:00Z", id: "comment-4" }),
    reply({ body: "Newest reply", created_at: "2026-09-09T00:05:00Z", id: "comment-5" }),
  ];
  const thread = async () => ({
    ask: input,
    answers: [],
    claimant: null,
    edits: [],
    followers: [],
    replies,
    route: null,
    route_status: null,
    route_holder: null,
    route_status_reason: null,
  });
  const { view } = renderCard(
    <AskCard
      ask={input}
      createReply={async () => {
        const fresh = reply({
          body: "Fresh reply",
          created_at: "2026-09-09T00:06:00Z",
          id: "comment-6",
        });
        replies = [...replies, fresh];
        return fresh;
      }}
      getAskThread={thread}
      thread="collapsed"
    />
  );

  try {
    const threadElement = await view.findByTestId("thread-ask-1");
    await waitFor(() =>
      expect(shownReplyBodies(threadElement)).toEqual(["Newer reply", "Newest reply"])
    );
    expect(view.queryByText("Middle reply")).not.toBeInTheDocument();

    const showMore = view.getByRole("button", { name: "Show 3 earlier replies" });
    expect(showMore).toHaveAttribute("aria-expanded", "false");
    expect(showMore.getAttribute("aria-controls")).not.toBeNull();
    showMore.focus();
    await user.click(showMore);
    await waitFor(() =>
      expect(shownReplyBodies(threadElement)).toEqual([
        "Oldest reply",
        "Older reply",
        "Middle reply",
        "Newer reply",
        "Newest reply",
      ])
    );
    const showFewer = view.getByRole("button", { name: "Show fewer replies" });
    expect(showFewer).toHaveFocus();
    expect(showFewer).toHaveAttribute("aria-expanded", "true");
    expect(controlledBy(showFewer)?.textContent).toContain("Middle reply");
    await user.click(showFewer);
    await waitFor(() =>
      expect(shownReplyBodies(threadElement)).toEqual(["Newer reply", "Newest reply"])
    );

    await user.click(view.getByRole("button", { name: "Write a reply" }));
    const replyField = view.getByLabelText("Reply");
    await user.click(replyField);
    await user.paste("Fresh reply");
    // The form's own submit, which user-event has no form for.
    fireEvent.submit(replyField.closest("form") as HTMLFormElement);
    await waitFor(() =>
      expect(shownReplyBodies(threadElement)).toEqual(["Newest reply", "Fresh reply"])
    );
  } finally {
    view.unmount();
  }
});

test("a compact answered ask's Write a reply reveals its composer as the next keyboard stop", async () => {
  const user = userEvent.setup();
  const input = answered(ask(), ["Ship"]);
  const { view } = renderCard(
    <AskCard
      ask={input}
      getAskThread={async () => ({
        ask: input,
        answers: [],
        edits: [],
        claimant: null,
        followers: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
        replies: [reply({ body: "Earlier reply" })],
      })}
      thread="collapsed"
    />
  );

  try {
    await view.findByText("Earlier reply");
    const replyToggle = view.getByRole("button", { name: "Write a reply" });
    // Enter or Space on a native button activates it as a click; focus stays on the button.
    replyToggle.focus();
    await user.click(replyToggle);
    expect(replyToggle).toHaveAttribute("aria-expanded", "true");
    expect(replyToggle).toHaveFocus();

    const field = view.getByLabelText("Reply");
    nextTabStop(replyToggle)?.focus();
    expect(field).toHaveFocus();
    expect(controlledBy(replyToggle)?.contains(field)).toBe(true);
    // The toggle and the form's submit button have distinct names.
    expect(view.getAllByRole("button", { name: "Reply" })).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

test("a reply arriving while the earlier replies are shown lands last and keeps them shown", async () => {
  const user = userEvent.setup();
  const input = answered(ask(), ["Ship"]);
  let replies = [
    reply({ body: "Oldest reply", created_at: "2026-09-09T00:01:00Z", id: "comment-1" }),
    reply({ body: "Middle reply", created_at: "2026-09-09T00:02:00Z", id: "comment-2" }),
    reply({ body: "Newest reply", created_at: "2026-09-09T00:03:00Z", id: "comment-3" }),
  ];
  const { queryClient, view } = renderCard(
    <AskCard
      ask={input}
      getAskThread={async () => ({
        ask: input,
        answers: [],
        claimant: null,
        edits: [],
        followers: [],
        replies,
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
      })}
      thread="collapsed"
    />
  );

  try {
    const threadElement = await view.findByTestId("thread-ask-1");
    await user.click(await view.findByRole("button", { name: "Show 1 earlier reply" }));
    await waitFor(() =>
      expect(shownReplyBodies(threadElement)).toEqual([
        "Oldest reply",
        "Middle reply",
        "Newest reply",
      ])
    );

    // A comment event invalidates the ask's thread, as the live stream does.
    replies = [
      ...replies,
      reply({ body: "Arriving reply", created_at: "2026-09-09T00:04:00Z", id: "comment-4" }),
    ];
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["ask-thread", input.id] });
    });

    await waitFor(() =>
      expect(shownReplyBodies(threadElement)).toEqual([
        "Oldest reply",
        "Middle reply",
        "Newest reply",
        "Arriving reply",
      ])
    );
    const showFewer = view.getByRole("button", { name: "Show fewer replies" });
    expect(showFewer).toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
  }
});

test("a link to a folded reply opens the fold and still lets the reader collapse it", async () => {
  const user = userEvent.setup();
  const input = answered(ask(), ["Ship"]);
  const replies = [
    reply({ body: "Oldest reply", created_at: "2026-09-09T00:01:00Z", id: "comment-1" }),
    reply({ body: "Middle reply", created_at: "2026-09-09T00:02:00Z", id: "comment-2" }),
    reply({ body: "Newest reply", created_at: "2026-09-09T00:03:00Z", id: "comment-3" }),
  ];
  const { view } = renderCard(
    <AskCard
      ask={input}
      currentReplyId="comment-1"
      getAskThread={async () => ({
        ask: input,
        answers: [],
        claimant: null,
        edits: [],
        followers: [],
        replies,
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
      })}
      thread="collapsed"
    />
  );

  try {
    const threadElement = await view.findByTestId("thread-ask-1");
    // The link names a reply behind the fold, so the card lands with the fold open.
    await waitFor(() =>
      expect(shownReplyBodies(threadElement)).toEqual([
        "Oldest reply",
        "Middle reply",
        "Newest reply",
      ])
    );

    // The reader stays on that URL, so the link must not pin the fold open.
    await user.click(view.getByRole("button", { name: "Show fewer replies" }));
    await waitFor(() =>
      expect(shownReplyBodies(threadElement)).toEqual(["Middle reply", "Newest reply"])
    );
    const showMore = view.getByRole("button", { name: "Show 1 earlier reply" });
    expect(showMore).toHaveAttribute("aria-expanded", "false");

    await user.click(showMore);
    await waitFor(() =>
      expect(view.getByRole("button", { name: "Show fewer replies" })).toBeTruthy()
    );
  } finally {
    view.unmount();
  }
});

test("AskCard with no replies offers Write a reply only after an answer, and retries a failed thread fetch", async () => {
  const user = userEvent.setup();
  const openInput = ask();
  const { view: open } = renderCard(
    <AskCard ask={openInput} getAskThread={emptyThread(openInput)} thread="collapsed" />
  );
  const answeredInput = ask({
    id: "ask-answered",
    state: "answered",
    answer: { at: "2026-09-09T00:05:00Z", selected: ["Yes"], text: null, user: "alice" },
  });
  const { view: answered } = renderCard(
    <AskCard ask={answeredInput} getAskThread={emptyThread(answeredInput)} thread="collapsed" />
  );
  let attempts = 0;
  const failedInput = ask({ id: "ask-2" });
  const { view: failed } = renderCard(
    <AskCard
      ask={failedInput}
      getAskThread={async () => {
        attempts += 1;
        if (attempts === 1) throw new Error("boom");
        return {
          ask: failedInput,
          answers: [],
          claimant: null,
          edits: [],
          followers: [],
          replies: [],
          route: null,
          route_status: null,
          route_holder: null,
          route_status_reason: null,
        };
      }}
      thread="collapsed"
    />
  );

  try {
    const openCard = within(open.container);
    const answeredCard = within(answered.container);
    const failedCard = within(failed.container);
    await answeredCard.findByRole("button", { name: "Write a reply" });
    // The open ask's only composer is the card's own Answer / Ask back row.
    await openCard.findByRole("button", { name: "Ask back" });
    expect(openCard.queryByRole("button", { name: "Write a reply" })).not.toBeInTheDocument();
    const retry = await failedCard.findByRole("button", { name: "Replies unavailable — retry" });
    expect(retry).toHaveAttribute("title", "boom");

    await user.click(retry);
    await waitFor(() =>
      expect(
        failedCard.queryByRole("button", { name: "Replies unavailable — retry" })
      ).not.toBeInTheDocument()
    );
    expect(failedCard.queryByRole("button", { name: "Write a reply" })).not.toBeInTheDocument();
  } finally {
    open.unmount();
    answered.unmount();
    failed.unmount();
  }
});

test("a reply composer remounted while its send is out shows the draft disabled and posts once", async () => {
  const user = userEvent.setup();
  const input = answered(ask(), []);
  const sent = Promise.withResolvers<Comment>();
  const bodies: string[] = [];
  const createReply = async (_issueKey: string, body: CreateCommentInput) => {
    bodies.push(body.body);
    return sent.promise;
  };
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const composer = () => (
    <QueryClientProvider client={queryClient}>
      <AskReplyComposer ask={input} createReply={createReply} />
    </QueryClientProvider>
  );
  const first = render(composer());
  await user.click(first.getByRole("textbox", { name: "Reply" }));
  await user.paste("hello");
  await user.click(first.getByRole("button", { name: "Reply" }));
  await waitFor(() => expect(bodies).toEqual(["hello"]));
  first.unmount();

  // Write a reply closed and reopened, or a margin tab switch, mid-send.
  const second = render(composer());
  try {
    const field = second.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement;
    expect(field.value).toBe("hello");
    expect(field.disabled).toBe(true);
    const send = second.getByRole("button", { name: /^Repl/ }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
    // The send holds this one disabled, and user-event refuses a disabled control; the point of
    // this line is that the press the browser still delivers posts nothing a second time.
    fireEvent.click(send);

    await act(async () => sent.resolve(reply({ body: "hello" })));
    await waitFor(() => expect(field.value).toBe(""));
    expect(field.disabled).toBe(false);
    expect(bodies).toEqual(["hello"]);
  } finally {
    second.unmount();
  }
});

test("two reply composers for one ask share its draft and its send", async () => {
  const user = userEvent.setup();
  const input = answered(ask(), []);
  const sent = Promise.withResolvers<Comment>();
  const bodies: string[] = [];
  const createReply = async (_issueKey: string, body: CreateCommentInput) => {
    bodies.push(body.body);
    return sent.promise;
  };
  // The margin's inline composer and a Conversation or decision-block composer, on one page.
  const { view } = renderCard(
    <>
      <AskReplyComposer ask={input} createReply={createReply} />
      <AskReplyComposer ask={input} createReply={createReply} />
    </>
  );
  try {
    const [margin, block] = view.getAllByRole("textbox", {
      name: "Reply",
    }) as HTMLTextAreaElement[];
    await user.click(margin);
    await user.paste("One draft");
    expect(block.value).toBe("One draft");

    await user.click(view.getAllByRole("button", { name: "Reply" })[1] as HTMLElement);
    await waitFor(() => expect(bodies).toEqual(["One draft"]));
    expect(margin.disabled).toBe(true);
    expect(block.disabled).toBe(true);

    await act(async () => sent.resolve(reply({ body: "One draft" })));
    await waitFor(() => expect(margin.value).toBe(""));
    expect(block.value).toBe("");
    expect(bodies).toEqual(["One draft"]);
  } finally {
    view.unmount();
  }
});

// The reply under an ask takes a picture as the answer does: it uploads to the ask's issue, Reply
// waits for it, and its text joins the ask's shared draft as it stands when the upload lands.
test("a picture pasted into a reply composer joins its draft and holds the send until it lands", async () => {
  const user = userEvent.setup();
  const { promise: landed, resolve: land } = Promise.withResolvers<void>();
  const uploadArtifact = spyOn(api, "uploadArtifact").mockImplementation(async () => {
    await landed;
    return { artifact: { name: "shot.png", slug: "shot-png" }, version: { number: 1 } } as never;
  });
  const input = answered(ask(), []);
  const bodies: string[] = [];
  const createReply = async (_issueKey: string, body: CreateCommentInput) => {
    bodies.push(body.body);
    return reply({ body: body.body });
  };
  const { view } = renderCard(
    <AskReplyComposer ask={input} createReply={createReply} uploadOwner={{ issue: "CORE-1" }} />
  );
  try {
    const field = view.getByRole("textbox", { name: "Reply" }) as HTMLTextAreaElement;
    await user.click(field);
    await user.paste("Looks right");
    // user-event's paste carries text, never the file this row uploads.
    fireEvent.paste(field, { clipboardData: { files: [png("shot.png")] } });
    await view.findByText("Uploading file…");
    expect((view.getByRole("button", { name: "Reply" }) as HTMLButtonElement).disabled).toBe(true);
    expect(uploadArtifact.mock.calls[0]?.[0]).toEqual({ issue: "CORE-1" });
    // Typed while the upload is out: the picture lands after it, not over it. The draft is
    // replaced in one edit, since a clear would be an edit of its own for the upload to land on.
    await user.click(field);
    await user.keyboard("{Control>}a{/Control}");
    await user.paste("Looks right to me");

    land();
    await waitFor(() =>
      expect(field.value).toBe(
        "Looks right to me ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)"
      )
    );
    await user.click(view.getByRole("button", { name: "Reply" }));
    await waitFor(() =>
      expect(bodies).toEqual([
        "Looks right to me ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)",
      ])
    );
  } finally {
    view.unmount();
    uploadArtifact.mockRestore();
  }
});

test("a document ask links its project and document page", async () => {
  const input = ask({
    artifact_id: "artifact-1",
    document: { name: "Design notes", project: "CORE", slug: "design-notes" },
    issue_key: null,
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([
    {
      ...input,
      priority: null,
      snoozed_until: null,
      thread: {
        answers: [],
        claimant: null,
        edits: [],
        followers: [],
        replies: [],
        route: null,
        route_status: null,
        route_holder: null,
        route_status_reason: null,
      },
    },
  ]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({
    event_head: "0",
    kind: "user",
    login: "alice",
  });
  // The card the Inbox opens reads its document's subscribers, which this document has none of.
  const getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  // A document has no assignee, so the Inbox lists its question under the Unassigned filter
  // rather than in the reader's own default list.
  const view = render(
    <MemoryRouter initialEntries={["/?assignee=unassigned"]}>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("CORE · Design notes");
    expect(screen.getByRole("link", { name: "CORE · Design notes" })).toHaveAttribute(
      "href",
      "/projects/CORE/documents/design-notes?ask=ask-1"
    );
  } finally {
    view.unmount();
    getInbox.mockRestore();
    whoAmI.mockRestore();
    getArtifactSubscribers.mockRestore();
  }
});

test("answering a document ask invalidates the document and projects, not an issue", async () => {
  const user = userEvent.setup();
  const input = ask({
    artifact_id: "artifact-1",
    document: { name: "Design notes", project: "CORE", slug: "design-notes" },
    issue_key: null,
    options: [{ label: "Ship" }],
  });
  const { queryClient, view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async (_id, submission) => answered(input, submission.selected)}
      getAskThread={emptyThread(input)}
    />
  );
  const invalidate = spyOn(queryClient, "invalidateQueries");

  try {
    await user.click(await view.findByRole("radio", { name: "Ship" }));
    await user.click(view.getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
    const invalidated = invalidate.mock.calls.map((call) => call[0]?.queryKey);
    expect(invalidated).toContainEqual(["artifact", "artifact-1"]);
    expect(invalidated).toContainEqual(["projects"]);
    expect(invalidated).not.toContainEqual(["issue", null]);
    expect(invalidated).not.toContainEqual(["issues"]);
  } finally {
    invalidate.mockRestore();
    view.unmount();
  }
});

test("asking back from a document ask posts an artifact comment and keeps the selection", async () => {
  const user = userEvent.setup();
  const input = ask({
    artifact_id: "artifact-1",
    document: { name: "Design notes", project: "CORE", slug: "design-notes" },
    issue_key: null,
    options: [{ label: "Ship" }, { label: "Hold" }],
  });
  const createArtifactComment = spyOn(api, "createArtifactComment").mockResolvedValue(
    reply() as never
  );
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    const ship = await view.findByRole("radio", { name: "Ship" });
    await user.click(ship);
    const field = view.getByLabelText("Your answer");
    await user.click(field);
    await user.paste("Does this include the rollback?");
    await user.click(view.getByRole("button", { name: "Ask back" }));

    await waitFor(() =>
      expect(createArtifactComment).toHaveBeenCalledWith(
        "artifact-1",
        {
          ask_id: "ask-1",
          body: "Does this include the rollback?",
        },
        keyedAttempt
      )
    );
    await waitFor(() => expect(field).toHaveProperty("value", ""));
    expect((ship as HTMLInputElement).checked).toBe(true);
  } finally {
    view.unmount();
    createArtifactComment.mockRestore();
  }
});

test("AskCard compact variant keeps its note disclosure available after selecting an option", async () => {
  const user = userEvent.setup();
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard ask={input} getAskThread={emptyThread(input)} variant="compact" />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    const ship = await within(card).findByRole("radio", { name: "Ship" });
    await user.click(ship);
    expect((ship as HTMLInputElement).checked).toBe(true);
    expect((within(card).getByRole("radio", { name: "Hold" }) as HTMLInputElement).checked).toBe(
      false
    );
    await user.click(
      within(card).getByRole("button", { name: "Add a note or answer in your own words" })
    );
    expect(await within(card).findByLabelText("Your answer")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("the compact card's note disclosure leaves the reader in the field it reveals, and a close moves nowhere", async () => {
  const user = userEvent.setup();
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard ask={input} getAskThread={emptyThread(input)} variant="compact" />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    const disclosure = await within(card).findByRole("button", {
      name: "Add a note or answer in your own words",
    });
    // Whoever opens it - the reader's press here, the Inbox's Enter, which presses this same
    // control - is moved into the box behind it.
    await user.click(disclosure);
    const answer = within(card).getByLabelText("Your answer");
    expect(answer).toHaveFocus();

    // Picking an option that needs no words neither opens the disclosure nor takes the reader
    // off the option they just picked.
    await user.click(disclosure);
    expect(within(card).queryByLabelText("Your answer")).not.toBeInTheDocument();
    // The close moves nobody: the reader is left on the control they pressed, which a press
    // focuses.
    expect(disclosure).toHaveFocus();
    const ship = within(card).getByRole("radio", { name: "Ship" });
    await user.click(ship);
    expect(within(card).queryByLabelText("Your answer")).not.toBeInTheDocument();
    expect(ship).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("AskCard compact variant renders long options as the same wrapping choice rows as the full card", async () => {
  const user = userEvent.setup();
  const label =
    "one table per model: a row per published result, a column per selected scorer, cells the score with its confidence interval";
  const description =
    "Wrong grain: a publish is per engagement and a scorer setting is per engagement, so a per-model table repeats the same selection on every row and hides which engagement chose it.";
  const input = ask({
    options: [
      { description, label },
      { description: "Cheapest; every new engagement needs an engineer.", label: "**no** UI" },
    ],
  });
  const { view } = renderCard(
    <div style={{ width: 320 }}>
      <AskCard ask={input} getAskThread={emptyThread(input)} variant="compact" />
    </div>
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    const options = within(card).getByRole("group", { name: "Answer options" });
    const first = await within(options).findByRole("radio", { name: `${label} ${description}` });
    expect(within(options).getByRole("radio", { name: "Other" })).toBeTruthy();
    // Markdown in a label renders to its text, and the text is the radio's accessible name.
    expect(await within(options).findByRole("radio", { name: /^no UI/ })).toBeTruthy();
    expect(within(options).queryAllByRole("button")).toHaveLength(0);
    expect(
      options.querySelectorAll('[class*="whitespace-nowrap"], [class*="rounded-full"]')
    ).toHaveLength(0);
    expect(within(card).queryByRole("button", { name: "Answer" })).not.toBeInTheDocument();

    await user.click(first);
    expect((first as HTMLInputElement).checked).toBe(true);
    expect(within(card).getByRole("button", { name: "Answer" })).not.toHaveAttribute("disabled");
    expect(within(card).queryByLabelText("Your answer")).not.toBeInTheDocument();

    await user.click(within(options).getByRole("radio", { name: "Other" }));
    expect(await within(card).findByLabelText("Your answer")).toBeTruthy();
    expect(within(card).getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(
      true
    );
  } finally {
    view.unmount();
  }
});

test("AskCard compact variant keeps an open note visible when a real option is picked after typing", async () => {
  const user = userEvent.setup();
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const sent: AnswerAskInput[] = [];
  const { view } = renderCard(
    <AskCard
      answerAsk={async (_id, answerInput) => {
        sent.push(answerInput);
        return answered(input, answerInput.selected ?? [], answerInput.text ?? null);
      }}
      ask={input}
      getAskThread={emptyThread(input)}
      variant="compact"
    />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    await user.click(await within(card).findByRole("radio", { name: "Other" }));
    const field = await within(card).findByLabelText("Your answer");
    await user.click(field);
    await user.paste("Ship, but only after the freeze.");

    await user.click(within(card).getByRole("radio", { name: "Ship" }));
    // The note is still on screen - nothing typed is sent without being visible.
    const note = within(card).getByLabelText("Your answer");
    expect((note as HTMLTextAreaElement).value).toBe("Ship, but only after the freeze.");
    await user.click(within(card).getByRole("button", { name: "Answer" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toMatchObject({
      selected: ["Ship"],
      text: "Ship, but only after the freeze.",
    });
  } finally {
    view.unmount();
  }
});

test("AskCard compact variant hands focus to the row when Answer disables under it", async () => {
  const user = userEvent.setup();
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  let release: (value: Ask) => void = () => {};
  const { view } = renderCard(
    <div data-testid="row" tabIndex={-1}>
      <AskCard
        answerAsk={() =>
          new Promise<Ask>((resolve) => {
            release = resolve;
          })
        }
        ask={input}
        getAskThread={emptyThread(input)}
        variant="compact"
      />
    </div>
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    await user.click(await within(card).findByRole("radio", { name: "Ship" }));
    const answer = within(card).getByRole("button", { name: "Answer" });
    answer.focus();
    expect(answer).toHaveFocus();
    await user.click(answer);
    await waitFor(() => expect(answer.hasAttribute("disabled")).toBe(true));
    expect(view.getByTestId("row")).toHaveFocus();
    release(answered(input, ["Ship"]));
    await view.findByRole("img", { name: "Selected" });
  } finally {
    view.unmount();
  }
});

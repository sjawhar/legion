import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
import { Inbox } from "../features/inbox/Inbox";

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
    ...overrides,
  };
}

// Every test provides an explicit (usually empty) thread so mounting AskCard
// never falls through to the default getAskThread, which would issue a real
// fetch in this test environment.
function emptyThread(input: Ask): () => Promise<AskRead> {
  return async () => ({ ask: input, edits: [], followers: [], replies: [] });
}

function renderCard(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(<QueryClientProvider client={queryClient}>{node}</QueryClientProvider>);
  return { queryClient, view };
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
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/design-notes#b-decision-1");
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
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/artifacts/design-notes#b-section-1");
    expect(within(card).getByText("The chosen passage.", { exact: true })).toBeTruthy();
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
    expect(link.getAttribute("href")).toBe("/projects/CORE/documents/design-notes#b-section-1");
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
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/spec");
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
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/spec");
  } finally {
    view.unmount();
  }
});

test("AskCard exposes medium urgency to screen readers without an inline label", () => {
  const input = ask({ urgency: "med" });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    expect(view.getByRole("article", { name: "Urgency: Medium" })).toBeTruthy();
    expect(view.queryByText("Medium", { exact: true })).toBeNull();
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

test("AskCard copies its tmux target and confirms success", async () => {
  const originalClipboard = navigator.clipboard;
  const writeText = spyOn({ writeText: async () => undefined }, "writeText");
  Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
  const input = ask({
    author: { id: "session-1", kind: "session", origin: { tmux: "dev:4.7" } },
  });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    fireEvent.click(view.getByRole("button", { name: "Copy tmux target dev:4.7" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("dev:4.7"));
    expect(await view.findByText("Copied", { exact: true })).toBeTruthy();
  } finally {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: originalClipboard });
    view.unmount();
  }
});

test("AskCard confirms tmux target copy through the legacy clipboard fallback", async () => {
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
    fireEvent.click(view.getByRole("button", { name: "Copy tmux target dev:4.7" }));
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
    fireEvent.click(view.getByRole("button", { name: "Copy tmux target dev:4.7" }));
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
    fireEvent.click(
      view.getByRole("button", { name: "Copy reference dispatch://CORE-1/ask/ask-1" })
    );
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("dispatch://CORE-1/ask/ask-1"));
    fireEvent.click(
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
      getAskThread={async () => ({ ask: input, edits, followers: [], replies: [] })}
    />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    await waitFor(() => expect(card.textContent).toContain("Edited just now"));
    const summary = await view.findByText("Show 2 previous versions");
    const disclosure = summary.closest("details");
    if (!(disclosure instanceof HTMLDetailsElement)) {
      throw new Error("previous question disclosure is missing");
    }
    expect(disclosure.open).toBe(false);
    fireEvent.click(summary);
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

test("AskCard submits the selected single option with the revision the human reviewed", async () => {
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
    expect(answer.getAttribute("placeholder")).toBe(
      "Answer in your own words, or ask a question back"
    );
    const submit = view.getByRole("button", { name: "Answer" });
    expect(submit.hasAttribute("disabled")).toBe(true);

    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    expect(answer.getAttribute("placeholder")).toBe("Add a note (optional)");
    expect(submit.hasAttribute("disabled")).toBe(false);
    fireEvent.click(submit);

    await waitFor(() =>
      expect(submitted).toEqual([
        { id: "ask-1", input: { selected: ["Ship"], expected_edited_at: null } },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard submits every checked multiple option", async () => {
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
    fireEvent.click(await view.findByRole("checkbox", { name: "Docs" }));
    fireEvent.click(view.getByRole("checkbox", { name: "Tests" }));
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() =>
      expect(submitted).toEqual([{ selected: ["Docs", "Tests"], expected_edited_at: null }])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard submits free-text without inventing a selected option", async () => {
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
    fireEvent.change(view.getByLabelText("Your answer"), {
      target: { value: "Take the third path" },
    });
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

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
    fireEvent.change(field, { target: { value: "Take the third path" } });
    fireEvent.keyDown(field, { key: "Enter" });
    expect(submitted).toEqual([]);
    fireEvent.keyDown(field, { ctrlKey: true, key: "Enter" });

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
    fireEvent.click(other);
    expect(submit.hasAttribute("disabled")).toBe(true);
    fireEvent.change(view.getByLabelText("Your answer"), { target: { value: "Try a hybrid" } });
    expect(submit.hasAttribute("disabled")).toBe(false);
    fireEvent.click(submit);

    await waitFor(() =>
      expect(submitted).toEqual([{ selected: [], text: "Try a hybrid", expected_edited_at: null }])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard offers to ask back for a question-shaped answer without closing the ask", async () => {
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
    fireEvent.change(view.getByLabelText("Your answer"), {
      target: { value: "How does this fit our release plan?" },
    });
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

    const choice = await view.findByRole("group", { name: "Question-shaped answer" });
    const clarification = within(choice).getByRole("button", { name: "Ask back instead" });
    expect(answerCalls).toEqual([]);
    expect(document.activeElement).toBe(clarification);
    fireEvent.keyDown(clarification, { key: "Enter" });
    fireEvent.click(clarification);

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
    fireEvent.change(view.getByLabelText("Your answer"), { target: { value: "Why wait?" } });
    fireEvent.click(view.getByRole("button", { name: "Answer" }));
    fireEvent.click(await view.findByRole("button", { name: "Answer with it anyway" }));

    await waitFor(() =>
      expect(submitted).toEqual([{ selected: [], text: "Why wait?", expected_edited_at: null }])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard dismisses the question-shaped guard when the response becomes an answer", async () => {
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    const field = view.getByLabelText("Your answer");
    fireEvent.change(field, { target: { value: "Why wait?" } });
    fireEvent.click(view.getByRole("button", { name: "Answer" }));
    await view.findByRole("button", { name: "Ask back instead" });

    fireEvent.change(field, { target: { value: "Ship it." } });
    expect(view.queryByRole("button", { name: "Ask back instead" })).toBeNull();
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
    expect(within(card).queryByRole("radio", { name: "Other" })).toBeNull();
    expect(view.getByLabelText("Reason").getAttribute("placeholder")).toBe(
      "Answer in your own words, or ask a question back"
    );
  } finally {
    view.unmount();
  }
});

test("an approval-kind ask submits Approve with an optional note", async () => {
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
    fireEvent.click(await view.findByRole("radio", { name: "Approve" }));
    const reason = view.getByLabelText("Reason");
    expect(reason.getAttribute("placeholder")).toBe("Note (optional)");
    fireEvent.change(reason, { target: { value: "Ready to ship." } });
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() =>
      expect(submitted).toEqual([
        { selected: ["Approve"], text: "Ready to ship.", expected_edited_at: null },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("an approval-kind ask names the reason Request changes needs, focuses its field, and says why Answer is dead", async () => {
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
    expect(view.queryByText("Add a reason to send Request changes")).toBeNull();
    fireEvent.click(await view.findByRole("radio", { name: "Request changes" }));
    const reasonField = view.getByLabelText("Reason (required)");
    expect(document.activeElement).toBe(reasonField);
    const submit = view.getByRole("button", { name: "Answer" });
    expect(submit.hasAttribute("disabled")).toBe(true);
    expect(submit.getAttribute("title")).toBe("Add a reason to send Request changes");
    expect(view.getByText("Add a reason to send Request changes")).toBeTruthy();

    fireEvent.change(reasonField, { target: { value: "Needs another pass" } });
    expect(submit.hasAttribute("disabled")).toBe(false);
    expect(submit.hasAttribute("title")).toBe(false);
    expect(view.queryByText("Add a reason to send Request changes")).toBeNull();
    fireEvent.click(submit);

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
    expect(within(card).queryByText("Action", { exact: true })).toBeNull();
    fireEvent.click(await within(card).findByRole("radio", { name: "Can't" }));
    expect(within(card).getByRole("radio", { name: "Other" })).toBeTruthy();
    expect(within(card).queryByLabelText("Reason (required)")).toBeNull();
    const answer = within(card).getByRole("button", { name: "Answer" });
    expect(answer.hasAttribute("disabled")).toBe(false);
    expect(answer.hasAttribute("title")).toBe(false);
    fireEvent.click(answer);
    await waitFor(() =>
      expect(submitted).toEqual([{ selected: ["Can't"], expected_edited_at: null }])
    );
  } finally {
    view.unmount();
  }
});

test("the compact card opens and focuses the reason field when Request changes is picked, again when the disclosure reopens", async () => {
  const input = ask({
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
  });
  const { view } = renderCard(
    <AskCard ask={input} getAskThread={emptyThread(input)} variant="compact" />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    fireEvent.click(await within(card).findByRole("radio", { name: "Approve" }));
    expect(within(card).queryByLabelText(/Reason|Your answer/)).toBeNull();
    expect(within(card).getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(
      false
    );

    fireEvent.click(within(card).getByRole("radio", { name: "Request changes" }));
    const reason = within(card).getByLabelText("Reason (required)");
    expect(document.activeElement).toBe(reason);
    const answer = within(card).getByRole("button", { name: "Answer" });
    expect(answer.hasAttribute("disabled")).toBe(true);
    expect(within(card).getByText("Add a reason to send Request changes")).toBeTruthy();
    fireEvent.change(reason, { target: { value: "Needs another pass." } });
    expect(answer.hasAttribute("disabled")).toBe(false);

    // Closing the disclosure unmounts the field; Request changes is still the picked radio, so
    // reopening the disclosure is what brings the field back, and it must take focus again.
    fireEvent.click(
      within(card).getByRole("button", { name: "Add a note or answer in your own words" })
    );
    expect(within(card).queryByLabelText("Reason (required)")).toBeNull();
    fireEvent.click(
      within(card).getByRole("button", { name: "Add a note or answer in your own words" })
    );
    const reopened = within(card).getByLabelText("Reason (required)");
    expect(document.activeElement).toBe(reopened);
  } finally {
    view.unmount();
  }
});

test("AskCard submits selected options alongside a typed note", async () => {
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
    fireEvent.click(await view.findByRole("checkbox", { name: "Docs" }));
    fireEvent.change(view.getByLabelText("Your answer"), {
      target: { value: "Also update the wiki" },
    });
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

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
    fireEvent.click(await view.findByRole("checkbox", { name: "Docs" }));
    fireEvent.click(view.getByRole("checkbox", { name: "Other" }));
    const answer = view.getByRole("button", { name: "Answer" });
    expect(answer.hasAttribute("disabled")).toBe(true);
    fireEvent.change(view.getByLabelText("Your answer"), {
      target: { value: "Also update the guide." },
    });
    expect(answer.hasAttribute("disabled")).toBe(false);
    fireEvent.click(answer);

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
    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    fireEvent.change(view.getByLabelText("Your answer"), { target: { value: "After review." } });
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

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
  let rejectAnswer: (reason?: unknown) => void = () => {};
  const pendingAnswer = new Promise<Ask>((_resolve, reject) => {
    rejectAnswer = reject;
  });
  const input = ask({ options: [{ label: "Ship" }] });
  const { queryClient, view } = renderCard(
    <AskCard ask={input} answerAsk={() => pendingAnswer} getAskThread={emptyThread(input)} />
  );
  const inboxInput: InboxRow = { ...input, priority: null };
  queryClient.setQueryData<InboxRow[]>(["inbox"], [inboxInput]);

  try {
    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(queryClient.getQueryData<InboxRow[]>(["inbox"])).toEqual([]));
    rejectAnswer(new Error("offline"));
    await waitFor(() =>
      expect(queryClient.getQueryData<InboxRow[]>(["inbox"])).toEqual([inboxInput])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard reloads a changed question and requires the human to reconfirm", async () => {
  const input = ask({ options: [{ label: "Ship" }] });
  const current = ask({
    edited_at: "2026-09-12T12:00:00Z",
    options: [{ label: "Hold" }],
    question: "Should we hold this release?",
  });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async () => {
        throw new ApiError(409, { code: "ASK_EDITED", error: "question changed" });
      }}
      getAskThread={async () => ({ ask: current, edits: [], followers: [], replies: [] })}
    />
  );

  try {
    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

    await view.findByText(/This question changed while you were answering/);
    expect(view.getByText("Should we hold this release?")).toBeTruthy();
    expect(view.getByRole("radio", { name: "Hold" }).hasAttribute("checked")).toBe(false);
    expect(view.getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(true);
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
    expect(card.textContent).toContain("session:session-…");
  } finally {
    view.unmount();
  }
});

test("AskCard shows submitted free text in its answered card", async () => {
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
    fireEvent.click(await view.findByRole("radio", { name: /^Ship/ }));
    fireEvent.change(view.getByLabelText("Your answer"), { target: { value: "Proceed." } });
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
    const card = view.getByTestId("ask-ask-1");
    expect(card.textContent).toMatch(/Asked .* · Answered by alice/);
    expect(card.querySelectorAll("time")).toHaveLength(2);
    expect(within(card).getAllByRole("listitem")[0]?.dataset.selected).toBe("true");
    expect(view.getByText("Proceed.")).toBeTruthy();
    expect(view.queryByRole("button", { name: "Answer" })).toBeNull();
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
    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Answer" }));
    await view.findByRole("alert");
    fireEvent.click(view.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
    expect(attempts).toBe(2);
  } finally {
    view.unmount();
  }
});

test("AskCard ignores a same-task duplicate answer submit", async () => {
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
    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    const submit = view.getByRole("button", { name: "Answer" });
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
        edits: [],
        followers: [],
        replies: [reply({ id: "comment-1", body: "Any update?" })],
      })}
    />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    await waitFor(() => expect(card.textContent).toContain("Any update?"));
    expect(
      within(view.getByTestId("thread-ask-1")).getByText("alice", { exact: false })
    ).toBeTruthy();
    expect(view.queryByLabelText("Reply")).toBeNull();
    expect(view.queryByText("Replying does not answer the question.")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("AskCard keeps the thread and reply composer visible after the ask is answered", async () => {
  const input = ask({ options: [{ label: "Ship" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async () => answered(input, ["Ship"])}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

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
    expect(openScope.queryByLabelText("Reply")).toBeNull();

    expect(await answeredScope.findByLabelText("Reply")).toBeTruthy();
    expect(answeredScope.getByRole("button", { name: "Reply" })).toBeTruthy();
    expect(answeredScope.queryByText("Replying does not answer the question.")).toBeNull();
  } finally {
    openView.unmount();
    answeredView.unmount();
  }
});

test("AskCard asks back from its answer field and clears the text without clearing the selection", async () => {
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
    fireEvent.click(ship);
    const field = view.getByLabelText("Your answer");
    fireEvent.change(field, { target: { value: "Any update on this?" } });
    fireEvent.click(view.getByRole("button", { name: "Ask back" }));

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
    fireEvent.change(view.getByLabelText("Your answer"), {
      target: { value: "Retry me" },
    });
    fireEvent.click(view.getByRole("button", { name: "Ask back" }));
    const alert = await view.findByRole("alert");
    expect(alert.textContent).toContain("Could not send your clarification.");
    fireEvent.click(view.getByRole("button", { name: "Retry" }));

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
    expect(view.queryByRole("button", { name: "Reply" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a collapsed thread shows the reply count and expands to the thread on demand", async () => {
  const input = ask();
  const thread = async () => ({
    ask: input,
    edits: [],
    followers: [],
    replies: [reply({ body: "Any update?" }), reply({ body: "Soon.", id: "c2" })],
  });
  const { view } = renderCard(<AskCard ask={input} getAskThread={thread} thread="collapsed" />);

  try {
    const trigger = await view.findByRole("button", { name: "2 replies" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(view.queryByText("Any update?")).toBeNull();

    fireEvent.click(trigger);
    await waitFor(() => expect(view.getByText("Any update?")).toBeTruthy());
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(view.queryByLabelText("Reply")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a collapsed thread with no replies offers Reply only on an answered ask, and a failed thread fetch offers a retry", async () => {
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
        return { ask: failedInput, edits: [], followers: [], replies: [] };
      }}
      thread="collapsed"
    />
  );

  try {
    const openCard = within(open.container);
    const answeredCard = within(answered.container);
    const failedCard = within(failed.container);
    await answeredCard.findByRole("button", { name: "Reply" });
    // The open ask's only composer is the card's own Answer / Ask back row.
    await openCard.findByRole("button", { name: "Ask back" });
    expect(openCard.queryByRole("button", { name: "Reply" })).toBeNull();
    const retry = await failedCard.findByRole("button", { name: "Replies unavailable — retry" });
    expect(retry.getAttribute("title")).toBe("boom");

    fireEvent.click(retry);
    await waitFor(() =>
      expect(failedCard.queryByRole("button", { name: "Replies unavailable — retry" })).toBeNull()
    );
    expect(failedCard.queryByRole("button", { name: "Reply" })).toBeNull();
  } finally {
    open.unmount();
    answered.unmount();
    failed.unmount();
  }
});

test("a document ask links its project and document page", async () => {
  const input = ask({
    artifact_id: "artifact-1",
    document: { name: "Design notes", project: "CORE", slug: "design-notes" },
    issue_key: null,
  });
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([{ ...input, priority: null }]);
  const whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <Inbox />
      </QueryClientProvider>
    </MemoryRouter>
  );

  try {
    await screen.findByText("CORE · Design notes");
    expect(screen.getByRole("link", { name: "CORE · Design notes" }).getAttribute("href")).toBe(
      "/projects/CORE/documents/design-notes?ask=ask-1"
    );
  } finally {
    view.unmount();
    getInbox.mockRestore();
    whoAmI.mockRestore();
  }
});

test("answering a document ask invalidates the document and projects, not an issue", async () => {
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
    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Answer" }));
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
    fireEvent.click(ship);
    const field = view.getByLabelText("Your answer");
    fireEvent.change(field, { target: { value: "Does this include the rollback?" } });
    fireEvent.click(view.getByRole("button", { name: "Ask back" }));

    await waitFor(() =>
      expect(createArtifactComment).toHaveBeenCalledWith("artifact-1", {
        ask_id: "ask-1",
        body: "Does this include the rollback?",
      })
    );
    await waitFor(() => expect(field).toHaveProperty("value", ""));
    expect((ship as HTMLInputElement).checked).toBe(true);
  } finally {
    view.unmount();
    createArtifactComment.mockRestore();
  }
});

test("AskCard compact variant keeps its note disclosure available after selecting an option", async () => {
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard ask={input} getAskThread={emptyThread(input)} variant="compact" />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    const ship = await within(card).findByRole("radio", { name: "Ship" });
    fireEvent.click(ship);
    expect((ship as HTMLInputElement).checked).toBe(true);
    expect((within(card).getByRole("radio", { name: "Hold" }) as HTMLInputElement).checked).toBe(
      false
    );
    fireEvent.click(
      within(card).getByRole("button", { name: "Add a note or answer in your own words" })
    );
    expect(await within(card).findByLabelText("Your answer")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("AskCard compact variant renders long options as the same wrapping choice rows as the full card", async () => {
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
    expect(within(card).queryByRole("button", { name: "Answer" })).toBeNull();

    fireEvent.click(first);
    expect((first as HTMLInputElement).checked).toBe(true);
    expect(within(card).getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(
      false
    );
    expect(within(card).queryByLabelText("Your answer")).toBeNull();

    fireEvent.click(within(options).getByRole("radio", { name: "Other" }));
    expect(await within(card).findByLabelText("Your answer")).toBeTruthy();
    expect(within(card).getByRole("button", { name: "Answer" }).hasAttribute("disabled")).toBe(
      true
    );
  } finally {
    view.unmount();
  }
});

test("AskCard compact variant keeps an open note visible when a real option is picked after typing", async () => {
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
    fireEvent.click(await within(card).findByRole("radio", { name: "Other" }));
    const field = await within(card).findByLabelText("Your answer");
    fireEvent.change(field, { target: { value: "Ship, but only after the freeze." } });

    fireEvent.click(within(card).getByRole("radio", { name: "Ship" }));
    // The note is still on screen - nothing typed is sent without being visible.
    const note = within(card).getByLabelText("Your answer");
    expect((note as HTMLTextAreaElement).value).toBe("Ship, but only after the freeze.");
    fireEvent.click(within(card).getByRole("button", { name: "Answer" }));
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
    fireEvent.click(await within(card).findByRole("radio", { name: "Ship" }));
    const answer = within(card).getByRole("button", { name: "Answer" });
    answer.focus();
    expect(document.activeElement).toBe(answer);
    fireEvent.click(answer);
    await waitFor(() => expect(answer.hasAttribute("disabled")).toBe(true));
    expect(document.activeElement).toBe(view.getByTestId("row"));
    release(answered(input, ["Ship"]));
    await view.findByRole("img", { name: "Selected" });
  } finally {
    view.unmount();
  }
});

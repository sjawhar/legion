import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type {
  AnswerAskInput,
  Ask,
  AskEdit,
  AskRead,
  Comment,
  CreateCommentInput,
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
  return async () => ({ ask: input, edits: [], replies: [] });
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
    <AskCard ask={input} getAskThread={async () => ({ ask: input, edits, replies: [] })} />
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

test("AskCard submits the selected single option", async () => {
  const submitted: Array<{ id: string; input: { selected: string[]; text?: string } }> = [];
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
      expect(submitted).toEqual([{ id: "ask-1", input: { selected: ["Ship"] } }])
    );
  } finally {
    view.unmount();
  }
});

test("AskCard submits every checked multiple option", async () => {
  const submitted: Array<{ selected: string[] }> = [];
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

    await waitFor(() => expect(submitted).toEqual([{ selected: ["Docs", "Tests"] }]));
  } finally {
    view.unmount();
  }
});

test("AskCard submits free-text without inventing a selected option", async () => {
  const submitted: Array<{ selected: string[]; text?: string }> = [];
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

    await waitFor(() => expect(submitted).toEqual([{ selected: [], text: "Take the third path" }]));
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

    await waitFor(() => expect(submitted).toEqual([{ selected: [], text: "Take the third path" }]));
  } finally {
    view.unmount();
  }
});

test("AskCard submits typed free text without inventing a selected option", async () => {
  const submitted: Array<{ selected: string[]; text?: string }> = [];
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
    expect(submit.hasAttribute("disabled")).toBe(true);
    fireEvent.change(view.getByLabelText("Your answer"), { target: { value: "Try a hybrid" } });
    expect(submit.hasAttribute("disabled")).toBe(false);
    fireEvent.click(submit);

    await waitFor(() => expect(submitted).toEqual([{ selected: [], text: "Try a hybrid" }]));
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

    await waitFor(() => expect(submitted).toEqual([{ selected: [], text: "Why wait?" }]));
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
  const submitted: Array<{ selected: string[]; text?: string }> = [];
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
      expect(submitted).toEqual([{ selected: ["Approve"], text: "Ready to ship." }])
    );
  } finally {
    view.unmount();
  }
});

test("an approval-kind ask requires a reason before Request changes can submit", async () => {
  const submitted: Array<{ selected: string[]; text?: string }> = [];
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
    fireEvent.click(await view.findByRole("radio", { name: "Request changes" }));
    const reasonField = view.getByLabelText("Reason");
    const submit = view.getByRole("button", { name: "Answer" });
    expect(submit.hasAttribute("disabled")).toBe(true);

    fireEvent.change(reasonField, { target: { value: "Needs another pass" } });
    expect(submit.hasAttribute("disabled")).toBe(false);
    fireEvent.click(submit);

    await waitFor(() =>
      expect(submitted).toEqual([{ selected: ["Request changes"], text: "Needs another pass" }])
    );
  } finally {
    view.unmount();
  }
});

test("an action-kind ask keeps its age and fixed options in the shared answer form", async () => {
  const submitted: Array<{ selected: string[]; text?: string }> = [];
  const input = ask({
    created_at: new Date(Date.now() - 35 * 60 * 1000).toISOString(),
    kind: "action",
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
    expect(within(card).getByText("Action", { exact: true })).toBeTruthy();
    expect(within(card).getByText("35m", { exact: true })).toBeTruthy();
    const cannot = await within(card).findByRole("radio", { name: "Can't" });
    fireEvent.click(cannot);
    const answer = within(card).getByRole("button", { name: "Answer" });
    expect(answer.hasAttribute("disabled")).toBe(true);
    fireEvent.change(within(card).getByLabelText("Your answer"), {
      target: { value: "The release permission is missing." },
    });
    fireEvent.click(answer);
    await waitFor(() =>
      expect(submitted).toEqual([
        { selected: ["Can't"], text: "The release permission is missing." },
      ])
    );
  } finally {
    view.unmount();
  }
});

test("an action-kind ask can ask back without losing a fixed option", async () => {
  const input = ask({
    kind: "action",
    options: [{ label: "Done" }, { label: "Can't" }],
  });
  const posted: CreateCommentInput[] = [];
  const { view } = renderCard(
    <AskCard
      ask={input}
      createReply={async (_issueKey, replyInput) => {
        posted.push(replyInput);
        return reply({ body: replyInput.body });
      }}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    const done = await view.findByRole("radio", { name: "Done" });
    fireEvent.click(done);
    const field = view.getByLabelText("Your answer");
    fireEvent.change(field, { target: { value: "Should I notify the team?" } });
    fireEvent.click(view.getByRole("button", { name: "Ask back" }));

    await waitFor(() =>
      expect(posted).toEqual([{ ask_id: "ask-1", body: "Should I notify the team?" }])
    );
    await waitFor(() => expect(field).toHaveProperty("value", ""));
    expect((done as HTMLInputElement).checked).toBe(true);
  } finally {
    view.unmount();
  }
});

test("AskCard submits selected options alongside a typed note", async () => {
  const submitted: Array<{ selected: string[]; text?: string }> = [];
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
      expect(submitted).toEqual([{ selected: ["Docs"], text: "Also update the wiki" }])
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
  queryClient.setQueryData<Ask[]>(["inbox"], [input]);

  try {
    fireEvent.click(await view.findByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Answer" }));

    await waitFor(() => expect(queryClient.getQueryData<Ask[]>(["inbox"])).toEqual([]));
    rejectAnswer(new Error("offline"));
    await waitFor(() => expect(queryClient.getQueryData<Ask[]>(["inbox"])).toEqual([input]));
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
    expect(card.textContent).toContain("session-1");
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
    const resolution = "Retracted by session-1 - Superseded.";
    await waitFor(() => expect(view.getByTestId("ask-ask-1").textContent).toContain(resolution));
    expect(view.getByTestId("thread-ask-1").textContent).not.toContain(resolution);
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
        return { ask: failedInput, edits: [], replies: [] };
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
  const getInbox = spyOn(api, "getInbox").mockResolvedValue([input]);
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

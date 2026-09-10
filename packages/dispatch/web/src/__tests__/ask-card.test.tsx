import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import type { Ask, AskRead, Comment } from "../api/types";
import { AskCard } from "../features/inbox/AskCard";

function ask(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: null,
    author: { kind: "session", id: "session-1" },
    created_at: "2026-09-09T00:00:00Z",
    id: "ask-1",
    issue_key: "CORE-1",
    multiple: false,
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

function reply(overrides: Partial<Comment> = {}): Comment {
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
    suggestion: null,
    ...overrides,
  };
}

// Every test provides an explicit (usually empty) thread so mounting AskCard
// never falls through to the default getAskThread, which would issue a real
// fetch in this test environment.
function emptyThread(input: Ask): () => Promise<AskRead> {
  return async () => ({ ask: input, replies: [] });
}

function renderCard(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(<QueryClientProvider client={queryClient}>{node}</QueryClientProvider>);
  return { queryClient, view };
}

// The same open anchored ask renders in more than one place at once (the issue board and
// the margin both show it) - each mounted AskCard's own answer field must stay independently
// labeled, not collide on an ask.id-derived id shared by every instance.
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
    expect(view.getByLabelText("Your answer")).toBeTruthy();
    const submit = view.getByRole("button", { name: "Submit answer" });
    expect(submit.hasAttribute("disabled")).toBe(true);

    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
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
    fireEvent.click(view.getByRole("checkbox", { name: "Docs" }));
    fireEvent.click(view.getByRole("checkbox", { name: "Tests" }));
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));

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
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));

    await waitFor(() => expect(submitted).toEqual([{ selected: [], text: "Take the third path" }]));
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
    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));

    await waitFor(() => expect(queryClient.getQueryData<Ask[]>(["inbox"])).toEqual([]));
    rejectAnswer(new Error("offline"));
    await waitFor(() => expect(queryClient.getQueryData<Ask[]>(["inbox"])).toEqual([input]));
  } finally {
    view.unmount();
  }
});

test("AskCard renders who answered, what was selected, and when, in place of the form", async () => {
  const now = spyOn(Date, "now").mockReturnValue(new Date("2026-09-09T00:06:00Z").getTime());
  const input = ask({ options: [{ label: "Ship" }, { label: "Hold" }] });
  const { view } = renderCard(
    <AskCard
      ask={input}
      answerAsk={async () => answered(input, ["Ship"])}
      getAskThread={emptyThread(input)}
    />
  );

  try {
    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));

    await waitFor(() => expect(view.getByText(/answered/)).toBeTruthy());
    expect(view.getByTestId("ask-ask-1").textContent).toContain("alice answered");
    expect(view.getByText(/Ship/)).toBeTruthy();
    expect(view.getByText("1 minute ago")).toBeTruthy();
    expect(view.getByText("1 minute ago").closest("time")?.getAttribute("dateTime")).toBe(
      "2026-09-09T00:05:00Z"
    );
    expect(view.queryByRole("button", { name: "Submit answer" })).toBeNull();
  } finally {
    now.mockRestore();
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
    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));
    await view.findByRole("alert");
    fireEvent.click(view.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(view.getByText(/answered/)).toBeTruthy());
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
    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
    const submit = view.getByRole("button", { name: "Submit answer" });
    fireEvent.click(submit);
    fireEvent.click(submit);

    await waitFor(() => expect(view.getByText(/answered/)).toBeTruthy());
    expect(attempts).toBe(1);
  } finally {
    view.unmount();
  }
});

test("AskCard renders the reply thread under the question before it is answered", async () => {
  const input = ask();
  const { view } = renderCard(
    <AskCard
      ask={input}
      getAskThread={async () => ({
        ask: input,
        replies: [reply({ id: "comment-1", body: "Any update?" })],
      })}
    />
  );

  try {
    await waitFor(() => expect(view.getByText("Any update?")).toBeTruthy());
    expect(
      within(view.getByTestId("thread-ask-1")).getByText("alice", { exact: false })
    ).toBeTruthy();
    // The composer stays available alongside existing replies.
    expect(view.getByRole("button", { name: "Reply" })).toBeTruthy();
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
    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));

    await waitFor(() => expect(view.getByText(/answered/)).toBeTruthy());
    // Answering is a distinct event, not the end of the conversation: the
    // thread section (and its composer) stays mounted below the answer.
    expect(view.getByRole("button", { name: "Reply" })).toBeTruthy();
    expect(view.getByLabelText("Reply")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("AskCard posts a reply to the ask and clears the composer on success", async () => {
  const input = ask();
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
    fireEvent.change(view.getByLabelText("Reply"), {
      target: { value: "Any update on this?" },
    });
    fireEvent.click(view.getByRole("button", { name: "Reply" }));

    await waitFor(() =>
      expect(posted).toEqual([{ issueKey: "CORE-1", body: "Any update on this?", askId: "ask-1" }])
    );
    await waitFor(() => expect(view.getByLabelText("Reply")).toHaveProperty("value", ""));
  } finally {
    view.unmount();
  }
});

test("AskCard surfaces a retryable error when posting a reply fails", async () => {
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
    fireEvent.change(view.getByLabelText("Reply"), { target: { value: "Retry me" } });
    fireEvent.click(view.getByRole("button", { name: "Reply" }));

    const alert = await view.findByRole("alert");
    expect(alert.textContent).toContain("Could not post your reply.");
    fireEvent.click(view.getByRole("button", { name: "Retry" }));

    await waitFor(() => expect(attempts).toBe(2));
  } finally {
    view.unmount();
  }
});

test("AskCard collapses a retracted ask into its resolution line", async () => {
  const input = {
    ...ask(),
    resolution: {
      actor: { kind: "session", id: "session-1" },
      at: "2026-09-10T00:01:00Z",
      kind: "retracted",
      reason: "A newer question supersedes this one.",
    },
    state: "resolved",
  } as unknown as Ask;
  const { view } = renderCard(<AskCard ask={input} getAskThread={emptyThread(input)} />);

  try {
    await waitFor(() =>
      expect(
        view.getByText("Retracted by session-1 - A newer question supersedes this one.")
      ).toBeTruthy()
    );
    expect(view.queryByRole("button", { name: "Submit answer" })).toBeNull();
    expect(view.queryByRole("button", { name: "Reply" })).toBeNull();
  } finally {
    view.unmount();
  }
});

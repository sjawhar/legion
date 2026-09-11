import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../api/client";
import type { Ask, AskRead, Comment, Event } from "../api/types";
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
test("AskCard marks an edited question and reveals its previous question and options", () => {
  const editedAt = new Date().toISOString();
  const input = ask({
    edited_at: editedAt,
    options: [{ label: "SSE" }],
    question: "Should we use SSE?",
  });
  const edited: Extract<Event, { type: "ask.edited" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: editedAt,
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      ...input,
      edited_by: { id: "session-1", kind: "session" },
      previous: {
        multiple: false,
        options: [{ label: "Polling" }],
        question: "Should we use polling?",
        urgency: "med",
      },
    },
    seq: 2,
    type: "ask.edited",
  };
  const earlier: Extract<Event, { type: "ask.edited" }> = {
    ...edited,
    created_at: "2026-09-10T00:00:00Z",
    id: 1,
    payload: {
      ...edited.payload,
      previous: {
        multiple: false,
        options: [{ label: "Email" }],
        question: "Should we use email?",
        urgency: "med",
      },
    },
    seq: 1,
  };
  const { view } = renderCard(
    <AskCard ask={input} events={[earlier, edited]} getAskThread={emptyThread(input)} />
  );

  try {
    const card = view.getByTestId("ask-ask-1");
    expect(card.textContent).toContain("Edited just now");
    const summary = view.getByText("Show previous question");
    const disclosure = summary.closest("details");
    if (!(disclosure instanceof HTMLDetailsElement)) {
      throw new Error("previous question disclosure is missing");
    }
    expect(disclosure.open).toBe(false);
    fireEvent.click(summary);
    expect(disclosure.open).toBe(true);
    expect(within(disclosure).getByText("Should we use polling?")).toBeTruthy();
    expect(within(disclosure).getByRole("list", { name: "Options" }).textContent).toContain(
      "Polling"
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

test("an answered ask lists every option, marks the selection, and shows when it was asked and answered", () => {
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
    expect(options[1]?.textContent).toContain("Wait for QA");

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
    fireEvent.click(view.getByRole("radio", { name: /^Ship/ }));
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
    const card = view.getByTestId("ask-ask-1");
    expect(card.textContent).toMatch(/Asked .* · Answered by alice/);
    expect(card.querySelectorAll("time")).toHaveLength(2);
    expect(within(card).getAllByRole("listitem")[0]?.dataset.selected).toBe("true");
    expect(view.getByText("Proceed.")).toBeTruthy();
    expect(view.queryByRole("button", { name: "Submit answer" })).toBeNull();
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
    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));
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
    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
    const submit = view.getByRole("button", { name: "Submit answer" });
    fireEvent.click(submit);
    fireEvent.click(submit);

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
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

    await waitFor(() => expect(view.getByText(/Answered by/)).toBeTruthy());
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

test("a resolved ask keeps its question and options and carries a resolution badge", () => {
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
    expect(view.getByText(input.question)).toBeTruthy();
    expect(view.getByRole("list", { name: "Options" }).children).toHaveLength(2);
    expect(view.getByTestId("ask-resolution-badge").textContent).toBe("Retracted");
    const resolution = "Retracted by session-1 - Superseded.";
    expect(view.getByTestId("ask-ask-1").textContent).toContain(resolution);
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
    expect(view.getByLabelText("Reply")).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a collapsed thread with no replies offers Reply, and a failed thread fetch offers a retry", async () => {
  const input = ask();
  const { view } = renderCard(
    <AskCard ask={input} getAskThread={emptyThread(input)} thread="collapsed" />
  );
  let attempts = 0;
  const failedInput = ask({ id: "ask-2" });
  const { view: failed } = renderCard(
    <AskCard
      ask={failedInput}
      getAskThread={async () => {
        attempts += 1;
        if (attempts === 1) throw new Error("boom");
        return { ask: failedInput, replies: [] };
      }}
      thread="collapsed"
    />
  );

  try {
    const first = within(view.container);
    const failedCard = within(failed.container);
    await first.findByRole("button", { name: "Reply" });
    const retry = await failedCard.findByRole("button", { name: "Replies unavailable — retry" });
    expect(retry.getAttribute("title")).toBe("boom");

    fireEvent.click(retry);
    await failedCard.findByRole("button", { name: "Reply" });
  } finally {
    view.unmount();
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
    fireEvent.click(view.getByRole("radio", { name: "Ship" }));
    fireEvent.click(view.getByRole("button", { name: "Submit answer" }));
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

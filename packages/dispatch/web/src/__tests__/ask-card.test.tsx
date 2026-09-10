import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import type { Ask } from "../api/types";
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

function renderCard(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(<QueryClientProvider client={queryClient}>{node}</QueryClientProvider>);
  return { queryClient, view };
}

test("AskCard submits the selected single option", async () => {
  const submitted: Array<{ id: string; input: { selected: string[]; text?: string } }> = [];
  const { view } = renderCard(
    <AskCard
      ask={ask({ options: [{ label: "Ship" }, { label: "Hold" }] })}
      answerAsk={async (id, input) => {
        submitted.push({ id, input });
        return ask({ id, state: "answered" });
      }}
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
  const { view } = renderCard(
    <AskCard
      ask={ask({ multiple: true, options: [{ label: "Docs" }, { label: "Tests" }] })}
      answerAsk={async (_id, input) => {
        submitted.push(input);
        return ask({ state: "answered" });
      }}
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
  const { view } = renderCard(
    <AskCard
      ask={ask()}
      answerAsk={async (_id, input) => {
        submitted.push(input);
        return ask({ state: "answered" });
      }}
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
  const { queryClient, view } = renderCard(<AskCard ask={input} answerAsk={() => pendingAnswer} />);
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
    <AskCard ask={input} answerAsk={async () => answered(input, ["Ship"])} />
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

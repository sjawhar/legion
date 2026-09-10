import { expect, test } from "bun:test";
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

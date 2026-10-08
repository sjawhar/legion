import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type { AnswerAskInput, Ask, AskAnswer, AskRead } from "../../api/types";
import { AskCard } from "./AskCard";

let whoAmI: Mock<typeof api.whoAmI>;
let getReferences: Mock<typeof api.getReferences>;

beforeEach(() => {
  whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
  getReferences = spyOn(api, "getReferences").mockResolvedValue({
    edges: [],
    node: { id: "ask-1", kind: "ask" },
  });
});

afterEach(() => {
  whoAmI.mockRestore();
  getReferences.mockRestore();
});

const firstAnswer: AskAnswer = {
  at: "2026-10-08T01:00:00Z",
  selected: ["Ship"],
  text: "First note",
  user: "alice",
};

function answeredAsk(overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer: firstAnswer,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-10-08T00:00:00Z",
    edited_at: null,
    id: "ask-1",
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [{ label: "Ship" }, { label: "Hold" }],
    question: "Which release path?",
    state: "answered",
    urgency: "med",
    waiting_on: "human",
    ...overrides,
  };
}

function thread(ask: Ask, answers: AskAnswer[] = ask.answer === null ? [] : [ask.answer]): AskRead {
  return { answers, ask, edits: [], followers: [], replies: [] };
}

function renderCard(node: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, view };
}

test("the answerer can change an answer and sees both answers immediately", async () => {
  const before = answeredAsk();
  const secondAnswer: AskAnswer = {
    at: "2026-10-08T02:00:00Z",
    selected: ["Hold"],
    text: null,
    user: "alice",
  };
  const after = answeredAsk({ answer: secondAnswer });
  const sent: AnswerAskInput[] = [];
  let threadReads = 0;
  const refreshed = Promise.withResolvers<AskRead>();
  const getAskThread = async () => {
    threadReads += 1;
    return refreshed.promise;
  };
  const { view } = renderCard(
    <AskCard
      answerAsk={async (_id, input) => {
        sent.push(input);
        return after;
      }}
      ask={before}
      getAskThread={getAskThread}
      initialThread={thread(before)}
    />
  );

  try {
    fireEvent.click(await view.findByRole("button", { name: "Change answer" }));
    const ship = (await view.findByRole("radio", { name: "Ship" })) as HTMLInputElement;
    expect(ship.checked).toBe(true);
    expect((view.getByLabelText("Your answer") as HTMLTextAreaElement).value).toBe("First note");
    fireEvent.click(view.getByRole("radio", { name: "Hold" }));
    fireEvent.change(view.getByLabelText("Your answer"), { target: { value: "" } });
    fireEvent.click(view.getByRole("button", { name: "Save answer" }));

    await waitFor(() =>
      expect(sent).toEqual([
        {
          expected_answer_at: firstAnswer.at,
          expected_edited_at: null,
          selected: ["Hold"],
        },
      ])
    );
    await waitFor(() => expect(view.getByText("Show 1 earlier answer")).toBeTruthy());
    expect(threadReads).toBe(1);
    fireEvent.click(view.getByText("Show 1 earlier answer"));
    expect(view.getByText("First note")).toBeTruthy();
    expect(
      within(view.getByTestId("ask-ask-1")).getAllByRole("listitem")[1]?.dataset.selected
    ).toBe("true");
    expect(threadReads).toBe(1);
    refreshed.resolve(thread(after, [firstAnswer, secondAnswer]));
    await waitFor(() => expect(view.getByText("Show 1 earlier answer")).toBeTruthy());
  } finally {
    view.unmount();
  }
});

test("Change answer is offered only to the person who gave a non-approval answer", async () => {
  const bob = renderCard(
    <AskCard
      ask={answeredAsk({ answer: { ...firstAnswer, user: "bob" } })}
      initialThread={thread(answeredAsk({ answer: { ...firstAnswer, user: "bob" } }))}
    />
  );
  try {
    await waitFor(() => expect(bob.view.getByText(/Answered by/)).toBeTruthy());
    expect(bob.view.queryByRole("button", { name: "Change answer" })).toBeNull();
  } finally {
    bob.view.unmount();
  }

  const approval = answeredAsk({
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
  });
  const review = renderCard(<AskCard ask={approval} initialThread={thread(approval)} />);
  try {
    await waitFor(() => expect(review.view.getByText(/Answered by/)).toBeTruthy());
    expect(review.view.queryByRole("button", { name: "Change answer" })).toBeNull();
  } finally {
    review.view.unmount();
  }
});

test("a stale change says the answer changed", async () => {
  const before = answeredAsk();
  const { view } = renderCard(
    <AskCard
      answerAsk={async () => {
        throw new ApiError(409, { code: "ASK_ANSWER_CHANGED", error: "answer changed" });
      }}
      ask={before}
      initialThread={thread(before)}
    />
  );

  try {
    fireEvent.click(await view.findByRole("button", { name: "Change answer" }));
    fireEvent.click(view.getByRole("radio", { name: "Hold" }));
    fireEvent.click(view.getByRole("button", { name: "Save answer" }));
    expect(
      await view.findByText("The answer changed since you opened it; here is the current one.")
    ).toBeTruthy();
    expect(view.queryByRole("button", { name: "Retry" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("mode change opens the seeded form only when the viewer can change the answer", async () => {
  const mine = answeredAsk();
  const editable = renderCard(<AskCard ask={mine} initialThread={thread(mine)} mode="change" />);
  try {
    expect(await editable.view.findByRole("button", { name: "Save answer" })).toBeTruthy();
    expect(editable.view.queryByText(/Answered by/)).toBeNull();
    const ship = editable.view.getByRole("radio", { name: "Ship" }) as HTMLInputElement;
    expect(ship.checked).toBe(true);
  } finally {
    editable.view.unmount();
  }

  const theirs = answeredAsk({ answer: { ...firstAnswer, user: "bob" } });
  const readOnly = renderCard(
    <AskCard ask={theirs} initialThread={thread(theirs)} mode="change" />
  );
  try {
    await waitFor(() => expect(readOnly.view.getByText(/Answered by/)).toBeTruthy());
    expect(readOnly.view.queryByRole("button", { name: "Save answer" })).toBeNull();
  } finally {
    readOnly.view.unmount();
  }
});

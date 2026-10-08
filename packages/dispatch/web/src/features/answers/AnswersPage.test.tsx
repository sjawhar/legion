import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  AnswerAskInput,
  Ask,
  AskAnswer,
  AskRead,
  MyAnswerRow,
  MyAnswersResponse,
} from "../../api/types";
import { AnswersPage } from "./AnswersPage";

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

const shipAnswer: AskAnswer = {
  at: "2026-10-08T03:00:00Z",
  selected: ["Ship"],
  text: "Go now.",
  user: "alice",
};

function answerRow(overrides: Partial<MyAnswerRow> = {}): MyAnswerRow {
  return {
    answer: shipAnswer,
    ask_id: "ask-1",
    ask_kind: "question",
    ask_state: "answered",
    at: shipAnswer.at,
    current: true,
    edited_at: null,
    kind: "answer",
    owner: { issue: { key: "CORE-1", title: "Release train" } },
    question: "Which release path?",
    ref: "/issues/CORE-1?ask=ask-1",
    ...overrides,
  };
}

function page(rows: MyAnswerRow[], total = rows.length, offset = 0): MyAnswersResponse {
  return { limit: 50, offset, rows, total };
}

function answeredAsk(answer: AskAnswer, overrides: Partial<Ask> = {}): Ask {
  return {
    anchor: null,
    answer,
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
    ...overrides,
  };
}

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  const view = render(
    <MemoryRouter initialEntries={["/answers"]}>
      <QueryClientProvider client={queryClient}>
        <AnswersPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { queryClient, view };
}

test("lists the person's answers and replies newest first with time, owner, question and answer", async () => {
  const listMyAnswers = spyOn(api, "listMyAnswers").mockResolvedValue(
    page([
      answerRow(),
      {
        ask_id: "ask-2",
        ask_kind: "question",
        ask_state: "open",
        at: "2026-10-08T02:00:00Z",
        current: false,
        edited_at: null,
        kind: "reply",
        owner: { document: { name: "Design notes", project: "CORE", slug: "design-notes" } },
        question: "Which colour?",
        ref: "/projects/CORE/documents/design-notes?ask=ask-2",
        reply: { body: "Does blue count?", id: "comment-1" },
      },
      answerRow({
        answer: { ...shipAnswer, at: "2026-10-08T01:00:00Z", selected: ["Hold"], text: null },
        ask_id: "ask-3",
        at: "2026-10-08T01:00:00Z",
        current: false,
        owner: { issue: { key: "CORE-3", title: "Billing" } },
        question: "Freeze billing?",
        ref: "/issues/CORE-3?ask=ask-3",
      }),
    ])
  );
  const { view } = renderPage();
  try {
    expect(await view.findByRole("heading", { name: "Answered by you" })).toBeTruthy();
    expect(view.getByRole("link", { name: "← Inbox" }).getAttribute("href")).toBe("/");
    expect(listMyAnswers).toHaveBeenCalledWith({ limit: 50, offset: 0 });

    const rows = view.getAllByRole("listitem").filter((row) => row.dataset.answerRow !== undefined);
    expect(rows.map((row) => row.dataset.answerRow)).toEqual(["ask-1", "ask-2", "ask-3"]);

    const [first, second, third] = rows as [HTMLElement, HTMLElement, HTMLElement];
    expect(first.querySelector("time")?.getAttribute("datetime")).toBe(shipAnswer.at);
    expect(
      within(first)
        .getByRole("link", { name: /CORE-1/ })
        .getAttribute("href")
    ).toBe("/issues/CORE-1");
    await waitFor(() => expect(within(first).getByText("Which release path?")).toBeTruthy());
    expect(within(first).getByText("Ship")).toBeTruthy();
    await waitFor(() => expect(within(first).getByText("Go now.")).toBeTruthy());
    expect(within(first).getByRole("link", { name: "Open" }).getAttribute("href")).toBe(
      "/issues/CORE-1?ask=ask-1"
    );
    expect(within(first).queryByText("Changed since")).toBeNull();
    expect(within(first).getByRole("button", { name: "Change answer" })).toBeTruthy();

    expect(
      within(second).getByRole("link", { name: "CORE · Design notes" }).getAttribute("href")
    ).toBe("/projects/CORE/documents/design-notes");
    expect(within(second).getByRole("link", { name: "Open" }).getAttribute("href")).toBe(
      "/projects/CORE/documents/design-notes?ask=ask-2"
    );
    await waitFor(() => expect(within(second).getByText(/Does blue count\?/)).toBeTruthy());
    expect(within(second).getByText("Replied:")).toBeTruthy();
    expect(within(second).queryByRole("button", { name: "Change answer" })).toBeNull();

    // An answer the person later replaced (or someone else's later answer) is history.
    expect(within(third).getByText("Changed since")).toBeTruthy();
    expect(within(third).queryByRole("button", { name: "Change answer" })).toBeNull();
  } finally {
    view.unmount();
    listMyAnswers.mockRestore();
  }
});

test("an approval answer offers no Change answer", async () => {
  const listMyAnswers = spyOn(api, "listMyAnswers").mockResolvedValue(
    page([
      answerRow({
        answer: { ...shipAnswer, selected: ["Approve"], text: null },
        ask_kind: "approval",
        question: "Approve spec (version 2)?",
      }),
    ])
  );
  const { view } = renderPage();
  try {
    await view.findByRole("link", { name: "Open" });
    expect(view.queryByRole("button", { name: "Change answer" })).toBeNull();
  } finally {
    view.unmount();
    listMyAnswers.mockRestore();
  }
});

test("says so when the person has answered nothing", async () => {
  const listMyAnswers = spyOn(api, "listMyAnswers").mockResolvedValue(page([]));
  const { view } = renderPage();
  try {
    expect(await view.findByText("You have not answered or replied on an ask yet.")).toBeTruthy();
  } finally {
    view.unmount();
    listMyAnswers.mockRestore();
  }
});

test("Show more appends the next page", async () => {
  const firstPage = Array.from({ length: 50 }, (_, index) =>
    answerRow({
      ask_id: `ask-${index}`,
      at: `2026-10-08T03:${String(59 - index).padStart(2, "0")}:00Z`,
    })
  );
  const listMyAnswers = spyOn(api, "listMyAnswers").mockImplementation(async ({ offset }) =>
    offset === 0
      ? page(firstPage, 51, 0)
      : page([answerRow({ ask_id: "ask-last", at: "2026-10-07T00:00:00Z" })], 51, 50)
  );
  const { view } = renderPage();
  try {
    fireEvent.click(await view.findByRole("button", { name: "Show more" }));
    await waitFor(() =>
      expect(view.container.querySelector('[data-answer-row="ask-last"]')).not.toBeNull()
    );
    expect(listMyAnswers).toHaveBeenLastCalledWith({ limit: 50, offset: 50 });
    expect(view.container.querySelectorAll("[data-answer-row]")).toHaveLength(51);
    expect(view.queryByRole("button", { name: "Show more" })).toBeNull();
  } finally {
    view.unmount();
    listMyAnswers.mockRestore();
  }
});

test("a row's Change answer opens the seeded form; after Save the row and the card show the change", async () => {
  const holdAnswer: AskAnswer = {
    at: "2026-10-08T04:00:00Z",
    selected: ["Hold"],
    text: null,
    user: "alice",
  };
  const before = answeredAsk(shipAnswer);
  const after = answeredAsk(holdAnswer);
  let changed = false;
  const listMyAnswers = spyOn(api, "listMyAnswers").mockImplementation(async () =>
    changed
      ? page([answerRow({ answer: holdAnswer, at: holdAnswer.at }), answerRow({ current: false })])
      : page([answerRow()])
  );
  const getAsk = spyOn(api, "getAsk").mockImplementation(
    async (): Promise<AskRead> =>
      changed
        ? { answers: [shipAnswer, holdAnswer], ask: after, edits: [], followers: [], replies: [] }
        : { answers: [shipAnswer], ask: before, edits: [], followers: [], replies: [] }
  );
  const sent: AnswerAskInput[] = [];
  const answerAsk = spyOn(api, "answerAsk").mockImplementation(async (_id, input) => {
    sent.push(input);
    changed = true;
    return after;
  });
  const { view } = renderPage();
  try {
    fireEvent.click(await view.findByRole("button", { name: "Change answer" }));
    const ship = (await view.findByRole("radio", { name: "Ship" })) as HTMLInputElement;
    expect(ship.checked).toBe(true);
    // The form opened, not a second completion card.
    const opened = view.container.querySelector<HTMLElement>('[data-answer-card="ask-1"]');
    if (opened === null) throw new Error("Change answer opened no card");
    expect(within(opened).queryByText(/Answered by/)).toBeNull();
    expect(within(opened).getByRole("button", { name: "Save answer" })).toBeTruthy();
    fireEvent.click(view.getByRole("radio", { name: "Hold" }));
    fireEvent.change(view.getByLabelText("Your answer"), { target: { value: "" } });
    fireEvent.click(view.getByRole("button", { name: "Save answer" }));

    await waitFor(() =>
      expect(sent).toEqual([
        { expected_answer_at: shipAnswer.at, expected_edited_at: null, selected: ["Hold"] },
      ])
    );
    // The row now carries the new answer, the old answer's row reads as history, and the card
    // that stays open below shows both answers in order.
    await waitFor(() =>
      expect(view.container.querySelectorAll('[data-answer-row="ask-1"]')).toHaveLength(2)
    );
    const rows = [...view.container.querySelectorAll<HTMLElement>('[data-answer-row="ask-1"]')];
    const [top, older] = rows as [HTMLElement, HTMLElement];
    expect(within(top).getByText("Hold")).toBeTruthy();
    expect(within(older).getByText("Changed since")).toBeTruthy();
    const card = view.container.querySelector<HTMLElement>('[data-answer-card="ask-1"]');
    if (card === null) throw new Error("the change card closed after Save");
    await waitFor(() => expect(within(card).getByText("Show 1 earlier answer")).toBeTruthy());
    expect(within(card).getByText(/Answered by/)).toBeTruthy();
    fireEvent.click(within(card).getByText("Show 1 earlier answer"));
    expect(within(card).getByText("Go now.")).toBeTruthy();
  } finally {
    view.unmount();
    listMyAnswers.mockRestore();
    getAsk.mockRestore();
    answerAsk.mockRestore();
  }
});

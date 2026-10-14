import { afterEach, beforeEach, expect, jest, type Mock, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { commentDeliveryFields } from "../../__tests__/comment-fixture";
import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { ApiError, api } from "../../api/client";
import type { AnswerAskInput, Ask, AskAnswer, AskRead, Comment } from "../../api/types";
import { seedUnread } from "../replies/replies-fixture";
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
  jest.useRealTimers();
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

for (const [code, message] of [
  ["NOT_ANSWERER", "Only the person who answered can change this answer."],
  ["ASK_APPROVAL_REVIEW", "An approval is a review; record a new review on the document."],
] as const) {
  test(`a change refused ${code} closes the form and shows the recorded answer`, async () => {
    const before = answeredAsk();
    const { view } = renderCard(
      <AskCard
        answerAsk={async () => {
          throw new ApiError(409, { code, error: "refused" });
        }}
        ask={before}
        initialThread={thread(before)}
      />
    );
    try {
      fireEvent.click(await view.findByRole("button", { name: "Change answer" }));
      fireEvent.click(view.getByRole("radio", { name: "Hold" }));
      fireEvent.click(view.getByRole("button", { name: "Save answer" }));
      expect(await view.findByText(message)).toBeTruthy();
      // The refusal meets the same send again, so the form it was sent from closes.
      expect(view.queryByRole("button", { name: "Save answer" })).toBeNull();
      expect(view.getByText(/Answered by/)).toBeTruthy();
      expect(view.queryByRole("button", { name: "Retry" })).toBeNull();
    } finally {
      view.unmount();
    }
  });
}

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

test("a compact card's change shows the seeded note, so nothing is sent unseen", async () => {
  const mine = answeredAsk();
  const { view } = renderCard(
    <AskCard
      ask={mine}
      frame="block"
      initialThread={thread(mine)}
      thread="collapsed"
      variant="compact"
    />
  );
  try {
    fireEvent.click(await view.findByRole("button", { name: "Change answer" }));
    const note = (await view.findByLabelText("Your answer")) as HTMLTextAreaElement;
    expect(note.value).toBe("First note");
    expect(view.getByRole("button", { name: "Save answer" })).toBeTruthy();
    expect(view.getByRole("button", { name: "Cancel" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

function askReply(id: string, createdAt: string): Comment {
  return {
    anchor: null,
    artifact_id: null,
    ask_id: "ask-1",
    author: { id: "session-1", kind: "session" },
    body: `Reply ${id}`,
    created_at: createdAt,
    edited_at: null,
    id,
    issue_key: "CORE-1",
    reply_to: null,
    resolved: false,
    resolved_at: null,
    resolved_by: null,
    suggestion: null,
    turn: null,
    ...commentDeliveryFields(),
  };
}

/** An answered ask with two replies, unread for alice on CORE-1, rendered with time and the
 *  screen in the test's hands: the replies follow the first answer, or precede `answer` when one
 *  is given. */
function renderUnreadAsk(props: {
  markRead: boolean;
  thread: "inline" | "collapsed";
  answer?: AskAnswer;
}) {
  jest.useFakeTimers();
  const screen = installIntersectionObserver();
  const mark = spyOn(api, "markThreadRead").mockResolvedValue(undefined);
  const ask = answeredAsk(props.answer === undefined ? {} : { answer: props.answer });
  const replies = [
    askReply("reply-1", "2026-10-08T03:00:00Z"),
    askReply("reply-2", "2026-10-08T04:00:00.5Z"),
  ];
  const queryClient = new QueryClient({
    defaultOptions: {
      mutations: { retry: false },
      queries: { retry: false, staleTime: Number.POSITIVE_INFINITY },
    },
  });
  seedUnread(queryClient, { issue: "CORE-1" }, [ask.id]);
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <AskCard
          ask={ask}
          initialThread={{ ...thread(ask), replies }}
          markRead={props.markRead}
          thread={props.thread}
          variant="compact"
        />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return {
    cleanup: () => {
      view.unmount();
      mark.mockRestore();
      screen.restore();
    },
    mark,
    screen,
    view,
  };
}

for (const shape of ["inline", "collapsed"] as const) {
  test(`with markRead, the ${shape} thread is marked read once its newest reply is on screen`, async () => {
    const card = renderUnreadAsk({ markRead: true, thread: shape });
    try {
      const newest = (await card.view.findByText("Reply reply-2")).closest("li");
      if (newest === null) throw new Error("expected the newest reply's item");
      expect(card.screen.observed()).toEqual([newest]);
      act(() => card.screen.show(newest));
      act(() => {
        jest.advanceTimersByTime(1_000);
      });
      for (let flush = 0; flush < 5; flush += 1) await act(async () => {});
      expect(card.mark.mock.calls).toEqual([["ask-1", "2026-10-08T04:00:00.5Z"]]);
    } finally {
      card.cleanup();
    }
  });

  test(`without markRead, the ${shape} thread watches nothing`, async () => {
    const card = renderUnreadAsk({ markRead: false, thread: shape });
    try {
      await card.view.findByText("Reply reply-2");
      expect(card.screen.observed()).toEqual([]);
      expect(card.mark).not.toHaveBeenCalled();
    } finally {
      card.cleanup();
    }
  });
}

// An answer is a write like a reply: when someone answers after every reply, the answered record is
// the newest write the card shows, so the mark watches the record and reads through the answer.
test("an answer given after every reply is what the mark watches and reads through", async () => {
  const answer: AskAnswer = {
    at: "2026-10-08T05:00:00.123456789Z",
    selected: ["Hold"],
    text: "Wait for the fix.",
    user: "bob@example.com",
  };
  const card = renderUnreadAsk({ answer, markRead: true, thread: "collapsed" });
  try {
    const record = await card.view.findByTestId("ask-ask-1");
    await card.view.findByText("Reply reply-2");
    expect(card.screen.observed()).toEqual([record]);
    act(() => card.screen.show(record));
    act(() => {
      jest.advanceTimersByTime(1_000);
    });
    for (let flush = 0; flush < 5; flush += 1) await act(async () => {});
    expect(card.mark.mock.calls).toEqual([["ask-1", answer.at]]);
  } finally {
    card.cleanup();
  }
});

/** One of the decision-block fixtures `cutDecisionBlocks` and the Go server's cut share. */
function decisionBlockFixture(name: string): string {
  return readFileSync(
    join(import.meta.dir, "../../../../../contracts/fixtures/decision-blocks", name),
    "utf8"
  );
}

const earlierApproval = {
  approved: { at: "2026-10-05T14:30:00Z", by: { id: "alice", kind: "user" }, version: 11 },
} satisfies Partial<NonNullable<Ask["approval"]>>;

/** An open approval request for version 12 of `spec.md`, carrying `approval` as given. */
function approvalAsk(approval: Partial<NonNullable<Ask["approval"]>> = {}): Ask {
  return answeredAsk({
    answer: null,
    approval: {
      artifact_id: "artifact-1",
      name: "spec.md",
      requested_version: 12,
      version: 12,
      ...approval,
    },
    approval_artifact: { name: "spec.md", primary: true, project: "CORE", slug: "spec-md" },
    kind: "approval",
    options: [{ label: "Approve" }, { label: "Request changes" }],
    question: "Approve spec.md (version 12)?",
    state: "open",
  });
}

/** `getArtifactVersion` answering `versions[n]` as version n's markdown. */
function mockVersions(versions: Record<number, string>) {
  return spyOn(api, "getArtifactVersion").mockImplementation(async (_id, version) => ({
    authors: [],
    created_at: "2026-10-05T00:00:00Z",
    markdown: versions[version] ?? "",
    named: false,
    number: version,
    summary: null,
  }));
}

// The suite's preload registers no testing-library cleanup, so a render another file never
// unmounts stays in the shared document; these tests read their own card alone (`within`).
for (const variant of ["full", "compact"] as const) {
  test(`a ${variant} approval card names the earlier approval and diffs the two versions only when asked`, async () => {
    const ask = approvalAsk(earlierApproval);
    const getArtifactVersion = mockVersions({ 11: "Before\n", 12: "After\n" });
    const { view } = renderCard(
      variant === "full" ? (
        <AskCard ask={ask} initialThread={thread(ask)} />
      ) : (
        <AskCard ask={ask} documentLink={false} initialThread={thread(ask)} variant="compact" />
      )
    );
    const card = within(view.container);
    try {
      const line = card.getByText(/^Approved at version 11 on/);
      expect(line.textContent).toBe("Approved at version 11 on 2026-10-05 by alice.");
      const time = line.querySelector("time");
      expect(time?.getAttribute("datetime")).toBe("2026-10-05T14:30:00Z");
      expect(time?.getAttribute("title")).toBe("2026-10-05T14:30:00Z");
      expect(card.getByText("Approve spec.md (version 12)?")).toBeTruthy();
      expect(card.queryByTestId("version-diff")).toBeNull();
      expect(getArtifactVersion).not.toHaveBeenCalled();

      const show = card.getByRole("button", { name: "Show diff" });
      expect(show.getAttribute("aria-expanded")).toBe("false");
      fireEvent.click(show);
      const diff = await card.findByTestId("version-diff");
      expect(getArtifactVersion.mock.calls).toEqual([
        ["artifact-1", 11],
        ["artifact-1", 12],
      ]);
      expect(diff.querySelector("del")?.textContent).toBe("Before\n");
      expect(diff.querySelector("ins")?.textContent).toBe("After\n");
      expect(card.queryByRole("button", { name: "Show diff" })).toBeNull();

      const hide = card.getByRole("button", { name: "Hide diff" });
      expect(hide.getAttribute("aria-expanded")).toBe("true");
      fireEvent.click(hide);
      expect(card.queryByTestId("version-diff")).toBeNull();
      expect(card.getByRole("button", { name: "Show diff" })).toBeTruthy();
    } finally {
      view.unmount();
      getArtifactVersion.mockRestore();
    }
  });
}

test("a first approval request shows no earlier approval and no diff control", async () => {
  const ask = approvalAsk();
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    await card.findByRole("radio", { name: "Approve" });
    expect(card.getByText("Approval requested")).toBeTruthy();
    expect(card.queryByText(/Approved at version/)).toBeNull();
    expect(card.queryByRole("button", { name: "Show diff" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("the diff cuts every decision block, so answers alone leave nothing changed", async () => {
  const ask = approvalAsk(earlierApproval);
  const getArtifactVersion = mockVersions({
    11: decisionBlockFixture("approved.md"),
    12: decisionBlockFixture("answered.md"),
  });
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    fireEvent.click(card.getByRole("button", { name: "Show diff" }));
    const diff = await card.findByTestId("version-diff");
    expect(diff.querySelector("ins, del")).toBeNull();
    expect(diff.textContent).toContain("The platform team asked for a rollback path.");
    expect(diff.textContent).not.toContain("ask{");
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

test("the diff keeps a paragraph changed beside the decision blocks", async () => {
  const ask = approvalAsk(earlierApproval);
  const getArtifactVersion = mockVersions({
    11: decisionBlockFixture("approved.md"),
    12: decisionBlockFixture("paragraph-changed.md"),
  });
  const { view } = renderCard(<AskCard ask={ask} initialThread={thread(ask)} />);
  const card = within(view.container);
  try {
    fireEvent.click(card.getByRole("button", { name: "Show diff" }));
    const diff = await card.findByTestId("version-diff");
    expect(diff.querySelector("del")?.textContent).toBe("The service ships behind a flag.\n");
    expect(diff.querySelector("ins")?.textContent).toBe(
      "The service ships behind a flag and a canary.\n"
    );
    expect(diff.textContent).not.toContain("ask{");
  } finally {
    view.unmount();
    getArtifactVersion.mockRestore();
  }
});

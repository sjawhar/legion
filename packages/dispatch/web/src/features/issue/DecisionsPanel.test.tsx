import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type {
  Artifact,
  Comment,
  Decision,
  DecisionKeep,
  IssueAskRow,
  IssueDecisions,
} from "../../api/types";
import { DecisionsPanel } from "./DecisionsPanel";

const documentId = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";
const askId = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const questionId = "qqqqqqqq-qqqq-4qqq-8qqq-qqqqqqqqqqqq";

const artifacts = [
  { id: "spec-id", issue_key: "CORE-1", kind: "doc", primary: true, project: "", slug: "spec" },
  {
    id: documentId,
    issue_key: "CORE-1",
    kind: "doc",
    name: "decisions.md",
    primary: false,
    project: "",
    slug: "decisions-md",
  },
] as unknown as Artifact[];

function decision(fields: Partial<Decision> & Pick<Decision, "block_id" | "heading" | "text">) {
  return {
    asks: [],
    changed_at: "2026-11-01T00:00:00Z",
    changed_version: 3,
    kept: null,
    kind: "agent",
    links: [],
    ref: `dispatch://CORE-1/artifact/decisions-md#b-${fields.block_id}`,
    replaced: false,
    text_sha256: `sum-${fields.block_id}`,
    unsure: false,
    waiting_review: false,
    ...fields,
  } as Decision;
}

const table = decision({
  block_id: "store",
  heading: "Store notices in a table",
  links: [
    {
      created_at: "2026-11-01T00:00:00Z",
      created_version: 1,
      from_block: "store-why",
      stale: false,
      to: { block_id: "p1", id: "CORE-1/spec#p1", kind: "block", text: "Notices are stored." },
    },
    {
      created_at: "2026-11-01T00:00:00Z",
      created_version: 1,
      from_block: "store-why",
      stale: true,
      to: { block_id: "p 2", id: "CORE-1/spec#p 2", kind: "block", text: "Old rows expire." },
    },
  ],
  text: "## Store notices in a table\n\nA table keeps them queryable.\n\nUnsure: the index may be slow.",
  unsure: true,
  waiting_review: true,
});
const named = decision({
  asks: [askId],
  block_id: "name",
  heading: "Name them notices",
  kind: "person",
  text: "## Name them notices\n\nThe person chose the name.",
});
const file = decision({
  block_id: "file",
  heading: "Keep them in a file",
  replaced: true,
  text: "### Keep them in a file\n\nA file was simpler.",
});

const view: IssueDecisions = {
  decisions: [table, named, file],
  document: "dispatch://CORE-1/artifact/decisions-md",
  open_questions: [
    {
      block_id: "q1",
      id: questionId,
      question: "Which index should the table carry?",
    } as unknown as IssueAskRow,
  ],
};

afterEach(cleanup);

function renderPanel() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient.setQueryData(["whoami"], { kind: "user", login: "alice" });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <DecisionsPanel artifacts={artifacts} isClosed={false} issueKey="CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
}

function list(name: string): HTMLElement {
  return screen.getByRole("region", { name });
}

test("the panel sorts the view into its four lists, in order", async () => {
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(view);
  try {
    renderPanel();
    await screen.findByRole("region", { name: "Open questions" });
    const headings = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(headings).toEqual([
      "Open questions",
      "Waiting for your review",
      "Your decisions",
      "Replaced",
    ]);

    const question = within(list("Open questions")).getByRole("link", {
      name: "Which index should the table carry?",
    });
    expect(question).toHaveAttribute("href", "/issues/CORE-1/spec#b-q1");

    const waiting = list("Waiting for your review");
    expect(within(waiting).getByText("Store notices in a table")).toBeDefined();
    expect(within(waiting).getByRole("button", { name: "Keep" })).toBeDefined();
    expect(within(waiting).getByRole("button", { name: "Change" })).toBeDefined();
    // The paragraph's markdown renders after the commit that mounts it, and the first body of
    // the run waits for the block schema and the headless engine to load.
    await waitFor(
      () =>
        expect(within(waiting).getByTestId("decision-unsure")).toHaveTextContent(
          "the index may be slow."
        ),
      { timeout: 10_000 }
    );
    // Each link opens the spec at the block it shaped, its id written as `encodeBlockId` writes it.
    const shaped = within(waiting)
      .getAllByRole("link")
      .map((link) => link.getAttribute("href"));
    expect(shaped).toEqual(["/issues/CORE-1/spec#b-p1", "/issues/CORE-1/spec#b-p%202"]);

    const mine = list("Your decisions");
    expect(within(mine).getByText("Name them notices")).toBeDefined();
    expect(within(mine).getByRole("link", { name: "The ask it answers" })).toHaveAttribute(
      "href",
      `/issues/CORE-1/asks/${askId}`
    );
    expect(within(mine).queryByRole("button", { name: "Keep" })).not.toBeInTheDocument();

    expect(within(list("Replaced")).getByText("Keep them in a file")).toBeDefined();
  } finally {
    read.mockRestore();
  }
});

test("Keep records the decision and moves it from Waiting for your review to Your decisions", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(view);
  const keep = spyOn(api, "keepDecision").mockResolvedValue({
    artifact_id: documentId,
    block_id: "store",
    changed_version: 3,
    kept_at: "2026-11-02T00:00:00Z",
    kept_by: "alice",
  } satisfies DecisionKeep);
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await user.click(within(waiting).getByRole("button", { name: "Keep" }));

    await waitFor(() => expect(keep).toHaveBeenCalled());
    expect(keep.mock.calls[0]?.slice(0, 3)).toEqual([documentId, "store", "sum-store"]);
    await waitFor(() =>
      expect(
        within(list("Waiting for your review")).queryByText("Store notices in a table")
      ).not.toBeInTheDocument()
    );
    expect(within(list("Your decisions")).getByText("Store notices in a table")).toBeDefined();
  } finally {
    read.mockRestore();
    keep.mockRestore();
  }
});

test("Change opens the comment composer on decisions.md, quoting the decision's first paragraph, and posts it as the issue's comment", async () => {
  const user = userEvent.setup();
  const read = spyOn(api, "getIssueDecisions").mockResolvedValue(view);
  const comment = spyOn(api, "createComment").mockResolvedValue({} as Comment);
  try {
    renderPanel();
    const waiting = await screen.findByRole("region", { name: "Waiting for your review" });
    await user.click(within(waiting).getByRole("button", { name: "Change" }));

    const composer = within(waiting).getByRole("form", { name: "Change Store notices in a table" });
    expect(within(composer).getByTestId("decision-quote").textContent).toBe(
      "A table keeps them queryable."
    );
    await user.click(within(composer).getByLabelText("Your change"));
    await user.paste("Use a queue instead.");
    await user.click(within(composer).getByRole("button", { name: "Send" }));

    await waitFor(() => expect(comment).toHaveBeenCalled());
    expect(comment.mock.calls[0]?.slice(0, 2)).toEqual([
      "CORE-1",
      {
        anchor: { artifact: documentId, quote: "A table keeps them queryable." },
        body: "Use a queue instead.",
      },
    ]);
  } finally {
    read.mockRestore();
    comment.mockRestore();
  }
});

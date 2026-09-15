import { expect, test } from "bun:test";
import { fireEvent, render, waitFor, within } from "@testing-library/react";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";

import type { Ask } from "../../api/types";
import { AskBlockCard } from "./AskBlockCard";
import type { AskBlockHost } from "./ask-block";

const schema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    bullet_list: { content: "list_item+", group: "block" },
    list_item: { content: "paragraph+" },
    ask: {
      attrs: {
        answer: { default: undefined },
        answered_at: { default: undefined },
        answered_by: { default: undefined },
        blockId: { default: null },
        invalid: { default: undefined },
        multiple: { default: false },
        selected: { default: undefined },
        state: { default: "open" },
        urgency: { default: "med" },
      },
      content: "paragraph+ bullet_list?",
      group: "block",
    },
  },
});

function askNode(
  attrs: Record<string, unknown>,
  options: string[] = ["Ship: Release it", "Hold"]
): ProseMirrorNode {
  return schema.node("ask", { blockId: "b-1", ...attrs }, [
    schema.node("paragraph", undefined, [schema.text("Should we ship?")]),
    ...(options.length === 0
      ? []
      : [
          schema.node(
            "bullet_list",
            undefined,
            options.map((option) =>
              schema.node("list_item", undefined, [
                schema.node("paragraph", undefined, [schema.text(option)]),
              ])
            )
          ),
        ]),
  ]);
}

const ask: Ask = {
  anchor: null,
  answer: null,
  author: {
    id: "01a086ad",
    kind: "session",
    origin: { session_title: "Architect for CORE-1" },
    owner: "sjawhar",
  },
  block_artifact: { id: "artifact-1", primary: true, slug: "spec" },
  block_id: "b-1",
  created_at: new Date(Date.now() - 3 * 60_000).toISOString(),
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ description: "Release it", label: "Ship" }, { label: "Hold" }],
  question: "Should we ship?",
  state: "open",
  urgency: "high",
};

function renderCard({
  ask: indexed = ask,
  indexed: hasIndexedAsk = true,
  node,
  onAnswer = () => {},
  pending = false,
  readOnly = false,
}: {
  ask?: Ask;
  /** Render as the server would before it has indexed the block: no ask row at all. */
  indexed?: boolean;
  node: ProseMirrorNode;
  onAnswer?: (ask: Ask, selected: string[], text: string) => void;
  pending?: boolean;
  readOnly?: boolean;
}) {
  const section = document.createElement("section");
  const header = document.createElement("div");
  const footer = document.createElement("div");
  section.append(header, footer);
  document.body.append(section);
  const host: AskBlockHost = { footer, header, key: 1, node };
  const view = render(
    <AskBlockCard
      ask={hasIndexedAsk ? indexed : undefined}
      host={host}
      onAnswer={onAnswer}
      owner={undefined}
      pending={pending}
      readOnly={readOnly}
    />
  );
  return {
    header: within(header),
    footer: within(footer),
    section,
    unmount() {
      view.unmount();
      section.remove();
    },
  };
}

test.each([
  ["blocking", "Blocking"],
  ["high", "High"],
  ["med", "Medium"],
  ["low", "Low"],
] as const)("an open %s decision names its urgency and who asked it", (urgency, label) => {
  const card = renderCard({ node: askNode({ urgency }) });
  try {
    const pill = card.section.querySelector("[data-dispatch-ask-pill]");
    expect(pill?.textContent).toBe(label);
    expect(pill?.getAttribute("data-dispatch-ask-pill")).toBe(urgency);
    expect(card.header.getByText("Decision")).toBeDefined();
    expect(card.section.querySelector("[data-dispatch-ask-asked]")?.textContent).toBe(
      "·asked by Architect for CORE-1 (for sjawhar)·3 minutes ago"
    );
  } finally {
    card.unmount();
  }
});

test("a block whose ask has no recorded author still says when it was asked", () => {
  const card = renderCard({
    ask: { ...ask, author: { id: "", kind: "user" } },
    node: askNode({ urgency: "med" }),
  });
  try {
    expect(card.section.querySelector("[data-dispatch-ask-asked]")?.textContent).toBe(
      "·asked3 minutes ago"
    );
  } finally {
    card.unmount();
  }
});

test("the options render as choice rows with their descriptions, plus Other; the submit waits for a choice", async () => {
  const answers: [string[], string][] = [];
  const card = renderCard({
    node: askNode({ urgency: "med" }),
    onAnswer: (_ask, selected, text) => answers.push([selected, text]),
  });
  try {
    // Option labels render through MarkdownBody, asynchronously.
    await waitFor(() =>
      expect(
        card.footer.getAllByRole("radio").map((input) => input.parentElement?.textContent)
      ).toEqual(["ShipRelease it", "Hold", "Other"])
    );
    const submit = card.footer.getByRole("button", { name: "Answer" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(card.footer.getByRole("radio", { name: "Ship Release it" }));
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(submit);
    expect(answers).toEqual([[["Ship"], ""]]);

    // Other needs words; the words are the answer, no option is recorded.
    fireEvent.click(card.footer.getByRole("radio", { name: "Other" }));
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(card.footer.getByLabelText("Your answer"), {
      target: { value: "Neither, actually." },
    });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(submit);
    expect(answers[1]).toEqual([[], "Neither, actually."]);
  } finally {
    card.unmount();
  }
});

test("with multiple choice, Other sits beside the options and both are sent", async () => {
  const answers: [string[], string][] = [];
  const card = renderCard({
    ask: { ...ask, multiple: true },
    node: askNode({ multiple: true, urgency: "high" }),
    onAnswer: (_ask, selected, text) => answers.push([selected, text]),
  });
  try {
    await waitFor(() => expect(card.footer.getAllByRole("checkbox")).toHaveLength(3));
    const submit = card.footer.getByRole("button", { name: "Answer" });
    fireEvent.click(card.footer.getByRole("checkbox", { name: "Ship Release it" }));
    fireEvent.click(card.footer.getByRole("checkbox", { name: "Other" }));
    // Other still needs words, even with an option ticked beside it.
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    expect(
      (card.footer.getByRole("checkbox", { name: "Ship Release it" }) as HTMLInputElement).checked
    ).toBe(true);
    fireEvent.change(card.footer.getByLabelText("Your answer"), {
      target: { value: "And a canary first." },
    });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(submit);
    expect(answers).toEqual([[["Ship"], "And a canary first."]]);
  } finally {
    card.unmount();
  }
});

test("a decision with no options answers in words alone", () => {
  const answers: [string[], string][] = [];
  const card = renderCard({
    ask: { ...ask, options: [] },
    node: askNode({ urgency: "low" }, []),
    onAnswer: (_ask, selected, text) => answers.push([selected, text]),
  });
  try {
    const submit = card.footer.getByRole("button", { name: "Answer" });
    expect(card.footer.queryAllByRole("radio")).toHaveLength(0);
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(card.footer.getByLabelText("Your answer"), { target: { value: " Go. " } });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.submit(card.section.querySelector("form") as HTMLFormElement);
    expect(answers).toEqual([[[], "Go."]]);
  } finally {
    card.unmount();
  }
});

test("the form stays disabled until the server has indexed the block into an ask", () => {
  const card = renderCard({ indexed: false, node: askNode({ urgency: "med" }) });
  try {
    expect(card.section.querySelector("[data-dispatch-ask-asked]")).toBeNull();
    // The whole form is fenced by its fieldset (browsers disable every descendant control).
    expect(card.section.querySelector("fieldset")?.hasAttribute("disabled")).toBe(true);
    expect(
      (card.footer.getByRole("button", { name: "Answer" }) as HTMLButtonElement).disabled
    ).toBe(true);
  } finally {
    card.unmount();
  }
});

test("a read-only surface shows an open decision's options with nothing to answer", () => {
  const card = renderCard({ node: askNode({ urgency: "med" }), readOnly: true });
  try {
    expect(card.section.querySelector("form")).toBeNull();
    expect(
      card.footer.getAllByRole("listitem").map((row) => (row as HTMLElement).dataset.selected)
    ).toEqual(["false", "false"]);
  } finally {
    card.unmount();
  }
});

test("an answered decision names who answered and when, ticks the chosen option, and shows the note", async () => {
  const card = renderCard({
    node: askNode({
      answer: "Ship it after the fix lands.",
      answered_at: new Date(Date.now() - 2 * 60 * 60_000).toISOString(),
      answered_by: "bob",
      selected: ["Ship"],
      state: "answered",
      urgency: "low",
    }),
  });
  try {
    expect(card.section.querySelector("form")).toBeNull();
    const answered = card.section.querySelector("[data-dispatch-ask-answered]");
    expect(answered?.textContent).toContain("Answered by bob");
    await waitFor(() => expect(answered?.querySelector("time")?.textContent).toBe("2 hours ago"));
    await waitFor(() =>
      expect(
        card.footer
          .getAllByRole("listitem")
          .map((row) => [(row as HTMLElement).dataset.selected, row.textContent])
      ).toEqual([
        ["true", "✓ShipRelease it"],
        ["false", "Hold"],
      ])
    );
    expect(card.section.querySelector("[data-dispatch-ask-answer]")?.textContent).toBe(
      "Ship it after the fix lands."
    );
  } finally {
    card.unmount();
  }
});

test("an Other answer is recorded under an Other label", () => {
  const card = renderCard({
    node: askNode({
      answer: "Neither.",
      answered_by: "bob",
      selected: [],
      state: "answered",
    }),
  });
  try {
    expect(card.footer.getByText("Other")).toBeDefined();
    expect(card.section.querySelector("[data-dispatch-ask-answer]")?.textContent).toBe("Neither.");
  } finally {
    card.unmount();
  }
});

test("a resolved decision shows its state and no form", () => {
  const card = renderCard({ node: askNode({ state: "resolved" }) });
  try {
    expect(card.section.querySelector("form")).toBeNull();
    expect(card.header.getByText("Resolved")).toBeDefined();
  } finally {
    card.unmount();
  }
});

test("a malformed decision says what is wrong and offers no form", () => {
  const card = renderCard({ node: askNode({ urgency: "high" }, ["Ship", ": no label"]) });
  try {
    expect(card.header.getByText("Malformed")).toBeDefined();
    expect(card.header.getByText("Option 2 has no label")).toBeDefined();
    expect(card.section.querySelector("form")).toBeNull();
    expect(
      card.footer.getByText("Fix the block text; the decision re-activates once it parses.")
    ).toBeDefined();
  } finally {
    card.unmount();
  }
});

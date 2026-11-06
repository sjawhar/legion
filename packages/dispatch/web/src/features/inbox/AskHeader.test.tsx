import { afterAll, afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, within } from "@testing-library/react";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";
import type { ReactNode } from "react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Ask } from "../../api/types";
import { AskBlockCard } from "../doc/AskBlockCard";
import type { AskBlockHost } from "../doc/typed-block-drawing";
import { AskCompletionCard } from "./AskCompletionCard";

/** Every header here names a session, which reads the registry; no test turns on a live one. */
const listAgents = spyOn(api, "listAgents");
const getAsk = spyOn(api, "getAsk");

afterEach(() => {
  listAgents.mockReset();
  getAsk.mockReset();
});

afterAll(() => {
  listAgents.mockRestore();
  getAsk.mockRestore();
});

const asker = {
  id: "01a086ad",
  kind: "session",
  origin: { session_title: "Architect for CORE-1" },
  owner: "sjawhar",
} as const;

const ask: Ask = {
  anchor: null,
  answer: null,
  author: asker,
  created_at: "2026-09-10T09:00:00Z",
  edited_at: null,
  id: "ask-1",
  issue_key: "CORE-1",
  kind: "question",
  multiple: false,
  opened_event_id: 1,
  options: [{ label: "Ship" }, { label: "Hold" }],
  question: "Should we ship?",
  state: "open",
  urgency: "blocking",
};

const answeredAsk: Ask = {
  ...ask,
  answer: {
    at: "2026-09-10T11:00:00Z",
    selected: ["Ship"],
    text: "Ship it after the fix lands.",
    user: "alice",
  },
  state: "answered",
};

const resolvedAsk: Ask = {
  ...ask,
  resolution: {
    actor: { id: "alice", kind: "user" },
    at: "2026-09-10T11:00:00Z",
    kind: "resolved",
    reason: "No longer needed.",
  },
  state: "resolved",
};

function renderCard(node: ReactNode) {
  listAgents.mockResolvedValue([]);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MemoryRouter>
  );
  return view;
}

/** The one header a surface draws, which every assertion here reads by its own attributes. */
function header(container: HTMLElement): HTMLElement {
  const found = container.querySelectorAll<HTMLElement>("[data-ask-header]");
  expect(found).toHaveLength(1);
  const one = found[0];
  if (one === undefined) throw new Error("no ask header");
  return one;
}

test("the answered record draws the urgency mark, the asker's name and the copy control", () => {
  const view = renderCard(
    <AskCompletionCard artifactSlug={undefined} ask={answeredAsk} edits={[]} />
  );
  try {
    const head = header(view.container);
    expect(head.dataset.askUrgency).toBe("blocking");
    expect(head.dataset.askState).toBe("answered");
    expect(head.dataset.askAsker).toBe("Architect for CORE-1 (for sjawhar)");
    expect(head.dataset.askAt).toBe("2026-09-10T09:00:00Z");
    expect(head.dataset.askCopy).toBe("dispatch://CORE-1/ask/ask-1");
    expect(within(head).getByText("BLOCKING")).toBeDefined();
    expect(within(head).getByRole("link", { name: "Architect for CORE-1" })).toBeDefined();
    expect(
      within(head).getByRole("button", { name: "Copy reference dispatch://CORE-1/ask/ask-1" })
    ).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("the resolved record draws the urgency mark and the copy control beside its state", () => {
  const view = renderCard(
    <AskCompletionCard artifactSlug={undefined} ask={resolvedAsk} edits={[]} />
  );
  try {
    const head = header(view.container);
    expect(head.dataset.askUrgency).toBe("blocking");
    expect(head.dataset.askState).toBe("resolved");
    expect(head.dataset.askAsker).toBe("Architect for CORE-1 (for sjawhar)");
    expect(head.dataset.askAt).toBe("2026-09-10T09:00:00Z");
    expect(head.dataset.askCopy).toBe("dispatch://CORE-1/ask/ask-1");
    expect(within(head).getByText("BLOCKING")).toBeDefined();
    expect(within(head).getByTestId("ask-resolution-badge").textContent).toBe("Resolved");
    expect(
      within(head).getByRole("button", { name: "Copy reference dispatch://CORE-1/ask/ask-1" })
    ).toBeDefined();
  } finally {
    view.unmount();
  }
});

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
        copied_from: { default: undefined },
        copied_from_document: { default: undefined },
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

function askNode(attrs: Record<string, unknown>): ProseMirrorNode {
  return schema.node("ask", { blockId: "b-1", ...attrs }, [
    schema.node("paragraph", undefined, [schema.text("Should we ship?")]),
    schema.node("bullet_list", undefined, [
      schema.node("list_item", undefined, [
        schema.node("paragraph", undefined, [schema.text("Ship")]),
      ]),
    ]),
  ]);
}

test("a decision block with no ask row shows one header with the fields its attributes hold", () => {
  const section = document.createElement("section");
  const head = document.createElement("div");
  const foot = document.createElement("div");
  section.append(head, foot);
  document.body.append(section);
  const host: AskBlockHost = {
    footer: foot,
    header: head,
    key: 1,
    node: askNode({
      answer: "Ship it after the fix lands.",
      answered_at: "2026-09-10T11:00:00Z",
      answered_by: "bob",
      selected: ["Ship"],
      state: "answered",
      urgency: "high",
    }),
  };
  const view = renderCard(
    <AskBlockCard
      ask={undefined}
      detached
      host={host}
      owner={undefined}
      readOnly
      rowResolution={false}
    />
  );
  try {
    const drawn = header(section);
    // The block's own attributes hold its urgency, its state and who answered it there - and no
    // asker, no time it was asked and no reference, so the header claims none of them.
    expect(drawn.dataset.askUrgency).toBe("high");
    expect(drawn.dataset.askState).toBe("answered");
    expect(drawn.dataset.askAsker).toBeUndefined();
    expect(drawn.dataset.askAt).toBeUndefined();
    expect(drawn.dataset.askCopy).toBeUndefined();
    expect(drawn.querySelector('[aria-label^="Copy reference"]')).toBeNull();
    expect(drawn.querySelector("[data-dispatch-ask-asked]")).toBeNull();
    expect(within(drawn).getByText("High")).toBeDefined();
    expect(drawn.textContent).toContain("Answered by bob");
    expect(getAsk).not.toHaveBeenCalled();
  } finally {
    view.unmount();
    section.remove();
  }
});

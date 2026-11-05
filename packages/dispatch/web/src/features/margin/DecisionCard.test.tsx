import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { Decision } from "../../api/types";
import { DecisionCard } from "./DecisionCard";
import type { DecisionMarginItem } from "./useMarginItems";

function item(decision: Partial<Decision>): DecisionMarginItem {
  const full: Decision = {
    asks: [],
    block_id: "d-1",
    changed_at: "2026-10-02T00:00:00Z",
    changed_version: 2,
    heading: "Store notices in a table",
    kept: null,
    kind: "agent",
    links: [],
    ref: "dispatch://CORE-1/spec#b-d-1",
    replaced: false,
    text: "A table keeps them through a restart.",
    text_sha256: "sum-d-1",
    uncertainty: "",
    uncertainty_reason: "",
    unsure: false,
    waiting_review: true,
    ...decision,
  };
  return {
    blockId: "p-plan",
    decision: full,
    id: `decision:${full.block_id}:p-plan`,
    kind: "decision",
  };
}

function renderCard(decision: Partial<Decision>, onSelect: (id: string) => void = () => {}) {
  return render(
    <MemoryRouter>
      <DecisionCard item={item(decision)} onSelect={onSelect} selected={false} />
    </MemoryRouter>
  );
}

test("a decision card names the decision, who made it, and opens its source node in the spec", async () => {
  const view = renderCard({});
  try {
    const card = screen.getByRole("article", { name: "Decision: Store notices in a table" });
    expect(card.getAttribute("data-margin-item")).toBe("decision:d-1:p-plan");
    expect(card.getAttribute("data-decision-unsure")).toBeNull();
    expect(screen.getByText("Agent's decision")).toBeDefined();
    expect(await screen.findByText("A table keeps them through a restart.")).toBeDefined();
    const link = screen.getByRole("link", { name: "Store notices in a table" });
    expect(link.getAttribute("href")).toBe("/issues/CORE-1/spec#b-d-1");
  } finally {
    view.unmount();
  }
});

test("an unsure decision is drawn in the unsure colour and says so; a person's says whose", () => {
  const view = renderCard({ kind: "person", unsure: true });
  try {
    const card = screen.getByRole("article", { name: "Decision: Store notices in a table" });
    expect(card.getAttribute("data-decision-unsure")).toBe("true");
    expect(screen.getByText("Unsure")).toBeDefined();
    expect(screen.getByText("A person's decision")).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("a decision an agent made alone names how sure it is", () => {
  const view = renderCard({ uncertainty: "high", unsure: true });
  try {
    expect(screen.getByText("Unsure: high")).toBeDefined();
  } finally {
    view.unmount();
  }
});

test("selecting a decision card selects its margin item", () => {
  const selected: string[] = [];
  const view = renderCard({}, (id) => selected.push(id));
  try {
    fireEvent.click(screen.getByRole("article", { name: "Decision: Store notices in a table" }));
    expect(selected).toEqual(["decision:d-1:p-plan"]);
  } finally {
    view.unmount();
  }
});

import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { Decision, Interpretation, IssueDecisions } from "../../api/types";
import { DecisionsPanel } from "./DecisionsPanel";

afterEach(cleanup);

const choice: Decision = {
  asks: [],
  block_id: "choice",
  changed_at: null,
  changed_version: 1,
  heading: "Keep the original name",
  kept: null,
  kind: "agent",
  links: [],
  ref: "dispatch://CORE-1/spec#b-choice",
  replaced: false,
  text: "## Keep the original name\n\nThe agent's own choice.",
  text_sha256: "choice-hash",
  unsure: false,
  waiting_review: true,
};

test("interpretations name a trusted agent author without attributing handmade text to an agent", async () => {
  const view: IssueDecisions = {
    decisions: [choice],
    document: "dispatch://CORE-1/spec",
    open_questions: [],
  };
  const items: Interpretation[] = [
    {
      block_id: "agent",
      decision_ref: choice.ref,
      ref: "dispatch://CORE-1/spec#b-agent",
      text: "The agent's reading.",
      author: { kind: "session", id: "reading-agent" },
    },
    {
      block_id: "handmade",
      decision_ref: choice.ref,
      ref: "dispatch://CORE-1/spec#b-handmade",
      text: "The hand-made reading.",
      author: null,
    },
    {
      block_id: "elsewhere",
      decision_ref: "dispatch://CORE-2/spec#b-choice",
      ref: "dispatch://CORE-2/spec#b-elsewhere",
      text: "Another issue's interpretation.",
      author: null,
    },
  ];
  const decisions = spyOn(api, "getIssueDecisions").mockResolvedValue(view);
  const interpretations = spyOn(api, "getIssueInterpretations").mockResolvedValue(items);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["whoami"], { kind: "user", login: "alice" });
  try {
    const rendered = render(
      <MemoryRouter>
        <QueryClientProvider client={client}>
          <DecisionsPanel artifacts={[]} isClosed={false} issueKey="CORE-1" />
        </QueryClientProvider>
      </MemoryRouter>
    );
    const children = await screen.findByRole("list", { name: "Interpretations" });
    expect(within(children).getByText("Agent interpretation — reading-agent")).toBeDefined();
    expect(within(children).getByText("Interpretation", { exact: true })).toBeDefined();
    expect(within(children).getAllByTestId("decision-interpretation")).toHaveLength(2);
    expect(screen.queryByText("Another issue's interpretation.")).toBeNull();
    expect(rendered.container.querySelectorAll("[data-decision]")).toHaveLength(1);
    expect(interpretations).toHaveBeenCalledWith("CORE-1");
  } finally {
    decisions.mockRestore();
    interpretations.mockRestore();
    client.clear();
  }
});

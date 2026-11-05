import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { api } from "../../api/client";
import type { IssueCoverage } from "../../api/types";
import { SpecRequirements } from "./SpecRequirements";

function requirement(
  blockId: string,
  text: string,
  checks: number,
  planTasks: number,
  gaps: string[] = []
) {
  return {
    block_id: blockId,
    checks,
    gaps,
    plan_tasks: planTasks,
    ref: `dispatch://CORE-1/spec#b-${blockId}`,
    text,
  };
}

function renderRequirements(coverage: IssueCoverage) {
  const getIssueCoverage = spyOn(api, "getIssueCoverage").mockResolvedValue(coverage);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <SpecRequirements issueKey="CORE-1" />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return { getIssueCoverage, view };
}

let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
});

test("an uncovered requirement is flagged with what it lacks and a covered one is not", async () => {
  const { getIssueCoverage, view } = renderRequirements({
    gaps: ["r2 has no check", "r2 has no plan task"],
    has_plan: true,
    issue: "CORE-1",
    requirements: [
      requirement("r1", "Every issue page shows a Plan tab.", 1, 1),
      requirement("r2", "The Plan tab shows the plan review's approval.", 0, 0, [
        "has no check",
        "has no plan task",
      ]),
    ],
    spec_artifact_id: "artifact-1",
  });
  restore = () => {
    getIssueCoverage.mockRestore();
    view.unmount();
  };

  await screen.findByTestId("spec-requirements");
  const [covered, uncovered] = screen.getAllByTestId("spec-requirement") as [
    HTMLElement,
    HTMLElement,
  ];
  expect(covered.getAttribute("data-covered")).toBe("true");
  expect(within(covered).queryByTestId("spec-requirement-gap")).toBeNull();
  expect(uncovered.getAttribute("data-covered")).toBe("false");
  // The server's own words for the gap, with the block id it names stripped.
  expect(
    within(uncovered)
      .getAllByTestId("spec-requirement-gap")
      .map((flag) => flag.textContent)
  ).toEqual(["has no check", "has no plan task"]);
});

// A requirement with a check but no plan task is no gap until the issue has a plan document.
// The server decides that; the tab draws its answer rather than judging again.
test("a requirement with a check and no plan task carries no flag before the issue has a plan", async () => {
  const { getIssueCoverage, view } = renderRequirements({
    gaps: [],
    has_plan: false,
    issue: "CORE-1",
    requirements: [requirement("r1", "Every issue page shows a Plan tab.", 1, 0)],
    spec_artifact_id: "artifact-1",
  });
  restore = () => {
    getIssueCoverage.mockRestore();
    view.unmount();
  };

  await screen.findByTestId("spec-requirements");
  expect(screen.queryByTestId("spec-requirement-gap")).toBeNull();
});

test("a spec that marks no requirements says so", async () => {
  const { getIssueCoverage, view } = renderRequirements({
    gaps: ["the spec marks no requirements"],
    has_plan: false,
    issue: "CORE-1",
    requirements: [],
    spec_artifact_id: "artifact-1",
  });
  restore = () => {
    getIssueCoverage.mockRestore();
    view.unmount();
  };

  expect((await screen.findByTestId("spec-no-requirements")).textContent).toContain(
    "This spec marks no requirements."
  );
});

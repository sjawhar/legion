import { expect, test } from "bun:test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";

import type { DeliveryTimelineResponse } from "../../api/types";
import { FacetPanel } from "./FacetPanel";
import type { Filters } from "./lib/facets";

const noFilters: Filters = {
  repo: [],
  parentAgent: [],
  session: [],
  issue: [],
  priority: [],
  component: [],
  author: [],
  rework: [],
  deployed: [],
  search: "",
};

const emptyCounts = {
  repo: {},
  parent_agent: {},
  session: {},
  issue: {},
  priority: {},
  component: {},
  author: {},
  rework: {},
  deployed: {},
};

const data: DeliveryTimelineResponse = {
  color_counts: { repo: {}, author: {}, priority: {}, component: {}, parent_agent: {} },
  components: {
    "ACME/platform": { parent: null, title: "The platform" },
    "ACME/api": { parent: "ACME/platform", title: "Public API" },
    "ACME/api-auth": { parent: "ACME/api", title: "API auth" },
  },
  facet_counts: {
    ...emptyCounts,
    component: { "ACME/api": 2, "ACME/platform": 1, __no_issue__: 4, __no_component__: 3 },
    parent_agent: { "Agent 1": 2, __no_session__: 5 },
  },
  freshness: {
    last_error: null,
    last_event_at: null,
    last_reconcile_at: null,
    unfetchable_count: 0,
  },
  issue_titles: {},
  prs: [],
  runs: [],
  waiting: [],
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
};

function optionNames(listbox: string): (string | null)[] {
  return within(screen.getByRole("listbox", { name: listbox }))
    .getAllByRole("option")
    .map((option) => option.getAttribute("aria-label"));
}

test("a facet's options sort by count, read placeholders in words, and say a parent includes its sub-components", () => {
  render(<FacetPanel data={data} filters={noFilters} onChange={() => {}} />);
  try {
    fireEvent.click(screen.getByRole("button", { name: /^Component/ }));
    expect(optionNames("Component options")).toEqual([
      "No issue, 4 PRs",
      "No component, 3 PRs",
      "Public API (includes 1 sub-component), 2 PRs",
      "The platform (includes 2 sub-components), 1 PR",
    ]);
    fireEvent.click(screen.getByRole("button", { name: /^Parent agent/ }));
    expect(optionNames("Parent agent options")).toEqual(["No session, 5 PRs", "Agent 1, 2 PRs"]);
  } finally {
    cleanup();
  }
});

test("a facet's trigger names the facet and what is picked", () => {
  render(
    <FacetPanel
      data={data}
      filters={{ ...noFilters, component: ["ACME/api"] }}
      onChange={() => {}}
    />
  );
  try {
    expect(screen.getByRole("button", { name: "Component: 1 selected" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Parent agent: Any parent agent" })).toBeDefined();
  } finally {
    cleanup();
  }
});

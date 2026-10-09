import { afterEach, beforeEach, expect, type Mock, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ApiError, api } from "../../api/client";
import type {
  DeliveryMeasuresResponse,
  DeliveryPR,
  DeliverySettings,
  DeliveryTimelineResponse,
} from "../../api/types";
import { DeliveryPage } from "./DeliveryPage";

// The list view, so the page renders without the timeline chart, whose library needs a canvas.
const DELIVERY_URL =
  "/delivery?mode=list&from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

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

const emptyTimeline: DeliveryTimelineResponse = {
  color_counts: { repo: {}, author: {}, priority: {}, component: {}, parent_agent: {} },
  components: {},
  facet_counts: emptyCounts,
  freshness: {
    last_error: null,
    last_event_at: null,
    last_reconcile_at: null,
    unfetchable_count: 0,
  },
  issue_titles: {},
  prs: [],
  runs: [],
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
};

const notConfigured = new ApiError(404, {
  code: "DELIVERY_NOT_CONFIGURED",
  error: "delivery is not configured; set delivery_settings through PUT /api/v1/settings/delivery",
});

function renderPage(url = DELIVERY_URL) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <MemoryRouter initialEntries={[url]}>
      <QueryClientProvider client={queryClient}>
        <DeliveryPage />
      </QueryClientProvider>
    </MemoryRouter>
  );
  return queryClient;
}

/** The measures panel's read for the page's window: nothing in it, so every test that is not
 *  about the panel renders it without asserting on it. */
const emptyMeasures: DeliveryMeasuresResponse = {
  window: { from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" },
  computed_at: "2024-06-02T00:00:00Z",
  measures: {
    deploy_frequency: {
      successful_deploys: 0,
      deploys_with_prs: 0,
      per_day: 0,
      with_prs_per_day: 0,
    },
    lead_time: {
      merge_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      first_commit_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      opened_to_production: { median_minutes: null, p90_minutes: null, max_minutes: null },
      opened_to_merge: { median_minutes: null, p90_minutes: null, max_minutes: null },
    },
    change_failure_rate: {
      per_pr: { confirmed: 0, pending: 0, rejected: 0, reverts: 0, total: 0, rate: 0 },
      per_deploy: {
        confirmed: 0,
        pending: 0,
        rejected: 0,
        reverts: 0,
        total: 0,
        rate: 0,
        confirmed_or_pending: 0,
        upper_bound_rate: 0,
      },
    },
    time_to_restore: { median_minutes: null },
    rework_share: 0,
    deploy_run_success: { concluded: 0, reached_production: 0, cancelled: 0, rate: null },
    daily: [],
  },
  flags_source: "none",
  unowned_p0: [],
  targets: {
    deploys_per_day: 20,
    change_failure_rate: 0.05,
    merge_to_production_minutes: 45,
    opened_to_merge_median_minutes: 60,
    deploy_run_success_rate: 0.9,
    unowned_p0: 0,
  },
  status: {
    deploys_per_day: false,
    change_failure_rate: null,
    change_failure_rate_upper_bound: null,
    merge_to_production_median: null,
    merge_to_production: null,
    opened_to_merge: null,
    deploy_run_success: null,
    unowned_p0: true,
  },
  freshness: emptyTimeline.freshness,
};

let getDeliveryMeasures: Mock<typeof api.getDeliveryMeasures>;
beforeEach(() => {
  getDeliveryMeasures = spyOn(api, "getDeliveryMeasures").mockResolvedValue(emptyMeasures);
});
afterEach(() => {
  getDeliveryMeasures.mockRestore();
});

test("an unconfigured timeline shows the setup form in its place, reading no settings, and a save brings the timeline", async () => {
  const saved: DeliverySettings = {
    deploy_repo: "acme/widgets",
    deploy_workflow_path: ".github/workflows/deploy.yml",
    excluded_repos: [],
    last_error: null,
    last_event_at: null,
    last_reconcile_at: null,
    population_authors: ["octocat"],
    pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
    production_job_name: "widgets-release",
    updated_at: "2026-10-07T12:00:00Z",
    updated_by: { id: "alice@example.com", kind: "user" },
  };
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline")
    .mockRejectedValueOnce(notConfigured)
    .mockResolvedValue(emptyTimeline);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);
  const putDeliverySettings = spyOn(api, "putDeliverySettings").mockResolvedValue(saved);

  try {
    renderPage();

    expect(
      await screen.findByRole("heading", { name: "Set up the delivery timeline" })
    ).toBeDefined();
    expect(screen.queryByRole("button", { name: "Timeline" })).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    // The timeline's DELIVERY_NOT_CONFIGURED already says no record is stored: the form's fields
    // are there at once, empty, with no read of the record and no loading line.
    expect(screen.queryByText("Loading delivery settings…")).toBeNull();
    expect((screen.getByLabelText("Deploy repository") as HTMLInputElement).value).toBe("");

    fireEvent.change(screen.getByLabelText("Deploy repository"), {
      target: { value: "acme/widgets" },
    });
    fireEvent.change(screen.getByLabelText("Deploy workflow"), {
      target: { value: ".github/workflows/deploy.yml" },
    });
    fireEvent.change(screen.getByLabelText("Production job"), {
      target: { value: "widgets-release" },
    });
    fireEvent.change(screen.getByLabelText("PR checks workflow"), {
      target: { value: ".github/workflows/pr-checks.yml" },
    });
    fireEvent.change(screen.getByLabelText("Population authors"), {
      target: { value: "octocat" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save delivery settings" }));

    expect(await screen.findByText("No PRs in the current filter/window.")).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    expect(putDeliverySettings).toHaveBeenCalledTimes(1);
    expect(getDeliveryTimeline).toHaveBeenCalledTimes(2);
    expect(getDeliverySettings).not.toHaveBeenCalled();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
    getDeliverySettings.mockRestore();
    putDeliverySettings.mockRestore();
  }
});

test("a refetch of the unconfigured timeline, as tab focus starts, keeps the setup form and its unsaved draft", async () => {
  const refetch = Promise.withResolvers<DeliveryTimelineResponse>();
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline")
    .mockRejectedValueOnce(notConfigured)
    .mockReturnValueOnce(refetch.promise);

  try {
    const queryClient = renderPage();
    await screen.findByRole("heading", { name: "Set up the delivery timeline" });
    fireEvent.change(screen.getByLabelText("Deploy repository"), {
      target: { value: "acme/widgets" },
    });

    // Coming back to the tab (TanStack's focus listener is on `window`) refetches the timeline,
    // and while that refetch runs the query holds no error at all. Any render of the timeline's
    // body in between would unmount the form, so the draft surviving says none happened.
    window.dispatchEvent(new Event("visibilitychange"));
    await waitFor(() => expect(queryClient.isFetching()).toBe(1));
    expect(getDeliveryTimeline).toHaveBeenCalledTimes(2);
    expect((screen.getByLabelText("Deploy repository") as HTMLInputElement).value).toBe(
      "acme/widgets"
    );
    expect(screen.queryByRole("button", { name: "Timeline" })).toBeNull();
    expect(screen.queryByText("Loading delivery timeline…")).toBeNull();

    refetch.reject(notConfigured);
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));
    expect(screen.getByRole("heading", { name: "Set up the delivery timeline" })).toBeDefined();
    expect((screen.getByLabelText("Deploy repository") as HTMLInputElement).value).toBe(
      "acme/widgets"
    );
    expect(screen.queryByRole("button", { name: "Timeline" })).toBeNull();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("a refetch of the unconfigured timeline that fails otherwise shows that failure and Retry, not the setup form", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline")
    .mockRejectedValueOnce(notConfigured)
    .mockRejectedValueOnce(new ApiError(500, { code: "INTERNAL", error: "database unavailable" }));

  try {
    renderPage();
    await screen.findByRole("heading", { name: "Set up the delivery timeline" });

    window.dispatchEvent(new Event("visibilitychange"));

    expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    expect(getDeliveryTimeline).toHaveBeenCalledTimes(2);
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("a configured timeline renders with no setup form", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockResolvedValue(emptyTimeline);
  const getDeliverySettings = spyOn(api, "getDeliverySettings").mockResolvedValue(null);

  try {
    renderPage();

    expect(await screen.findByText("No PRs in the current filter/window.")).toBeDefined();
    expect(screen.getByRole("button", { name: "List" }).getAttribute("aria-pressed")).toBe("true");
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
    await waitFor(() => expect(getDeliveryTimeline).toHaveBeenCalledTimes(1));
    expect(getDeliverySettings).not.toHaveBeenCalled();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
    getDeliverySettings.mockRestore();
  }
});

test("any other timeline failure keeps the page and offers Retry, not the setup form", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockRejectedValue(
    new ApiError(503, { code: "INTERNAL", error: "database unavailable" })
  );

  try {
    renderPage();

    expect(await screen.findByText("Couldn't load the delivery timeline.")).toBeDefined();
    expect(screen.getByRole("button", { name: "Retry" })).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Set up the delivery timeline" })).toBeNull();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

const listedPR: DeliveryPR = {
  additions: 12,
  author: "octocat",
  components: ["ACME/api"],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 3,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: "ACME-1",
  issue_title: "Ship widgets",
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: "P0",
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: a waiting widget",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

test("the facet column offers each value with its count, sends a pick to the server and keeps the view in the URL", async () => {
  const timeline: DeliveryTimelineResponse = {
    ...emptyTimeline,
    components: { "ACME/api": { parent: null, title: "Public API" } },
    facet_counts: {
      ...emptyCounts,
      component: { "ACME/api": 1 },
      issue: { "ACME-1": 1 },
      priority: { P0: 1, __no_issue__: 3 },
      repo: { "acme/other": 5, "acme/widgets": 1 },
    },
    issue_titles: { "ACME-1": "Ship widgets" },
    prs: [listedPR],
  };
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockResolvedValue(timeline);

  try {
    renderPage();

    expect(await screen.findByText("1 PRs in current filter/window")).toBeDefined();
    expect(screen.getByRole("button", { name: "Repository" }).textContent).toBe("Any repository");
    fireEvent.click(screen.getByRole("button", { name: "Priority" }));
    const priorityOptions = within(screen.getByRole("listbox", { name: "Priority options" }));
    const options = priorityOptions.getAllByRole("option").map((option) => option.textContent);
    expect(options).toEqual(["No issue3", "P01"]);
    fireEvent.click(priorityOptions.getByRole("option", { name: /^P0/ }));

    await waitFor(() =>
      expect(getDeliveryTimeline).toHaveBeenLastCalledWith(
        expect.objectContaining({ priority: ["P0"], repo: [] })
      )
    );
    expect(screen.getByRole("button", { name: "Priority" }).textContent).toBe("1 selected");

    fireEvent.click(screen.getByRole("button", { name: "Dispatch issue" }));
    expect(screen.getByRole("option", { name: /ACME-1 — Ship widgets/ })).toBeDefined();

    const colorBy = screen.getByLabelText("Color merges by") as HTMLSelectElement;
    expect([...colorBy.options].map((option) => option.textContent)).toEqual([
      "Repository",
      "Author",
      "Priority",
      "Component",
      "Parent agent",
    ]);
    fireEvent.change(colorBy, { target: { value: "component" } });
    expect(colorBy.value).toBe("component");
    // The list's dot follows the colour-by facet, and the row reads its issue with its priority.
    expect(screen.getByText("ACME-1 · P0")).toBeDefined();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("the header names the window and the freshness row the prototype's six sources", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockResolvedValue({
    ...emptyTimeline,
    freshness: { ...emptyTimeline.freshness, last_error: "rate limited" },
  });
  try {
    renderPage();
    expect(await screen.findByRole("heading", { name: "Delivery timeline" })).toBeDefined();
    expect(screen.getByText(/^Generated .* · window/)).toBeDefined();
    // No brush window in the URL: nothing to clear.
    expect(screen.queryByRole("button", { name: "clear brush window" })).toBeNull();
    const row = screen.getByRole("status", { name: "Source freshness" });
    const sources = [...row.querySelectorAll("[data-source]")].map((node) =>
      node.getAttribute("data-source")
    );
    expect(sources).toEqual(["prs", "runs", "ci", "dispatch", "agents", "events"]);
    expect(within(row).getByText("Deploy runs never checked: rate limited")).toBeDefined();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("the list sorts by its headers and a row opens the PR's details", async () => {
  const shipped: DeliveryPR = {
    ...listedPR,
    deploy_run: 500,
    deployed_at: "2024-06-01T01:40:00Z",
    deployed_status: "deployed",
    id: "acme/widgets#2",
    merged_at: "2024-06-01T00:30:00Z",
    number: 2,
    title: "feat: a shipped widget",
    url: "https://github.com/acme/widgets/pull/2",
  };
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockResolvedValue({
    ...emptyTimeline,
    components: { "ACME/api": { parent: null, title: "Public API" } },
    prs: [listedPR, shipped],
  });
  try {
    renderPage();
    const titles = async () =>
      (await screen.findAllByRole("row"))
        .slice(1)
        .map((row) => (row instanceof HTMLTableRowElement ? row.cells[3]?.textContent : undefined));
    expect(await titles()).toEqual(["feat: a waiting widget", "feat: a shipped widget"]);
    fireEvent.click(screen.getByRole("button", { name: /^Merged/ }));
    expect(await titles()).toEqual(["feat: a shipped widget", "feat: a waiting widget"]);

    fireEvent.click(screen.getByText("feat: a shipped widget"));
    const details = await screen.findByRole("complementary", { name: "Details" });
    expect(within(details).getByRole("link", { name: "Open on GitHub" }).getAttribute("href")).toBe(
      "https://github.com/acme/widgets/pull/2"
    );
    expect(within(details).getByText("ACME-1 — Ship widgets")).toBeDefined();
    expect(within(details).getByText("Public API")).toBeDefined();
    expect(within(details).getByText("70 min")).toBeDefined();
    fireEvent.click(within(details).getByRole("button", { name: "Close details" }));
    expect(screen.queryByRole("complementary", { name: "Details" })).toBeNull();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("the measures panel sits between the freshness row and the view controls and reads the page's window and facets", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockResolvedValue({
    ...emptyTimeline,
    facet_counts: { ...emptyCounts, repo: { "acme/widgets": 1 } },
    prs: [listedPR],
  });
  getDeliveryMeasures.mockResolvedValue({
    ...emptyMeasures,
    unowned_p0: [{ key: "ACME-103", title: "Production deploy gate flakes" }],
  });
  try {
    renderPage();
    const p0 = await screen.findByRole("link", { name: "ACME-103" });
    expect(p0.getAttribute("href")).toBe("/issues/ACME-103");
    const freshness = screen.getByRole("status", { name: "Source freshness" });
    const panel = screen.getByLabelText("KPI targets");
    const controls = screen.getByRole("group", { name: "View" });
    expect(
      freshness.compareDocumentPosition(panel) & Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
    expect(panel.compareDocumentPosition(controls) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(getDeliveryMeasures).toHaveBeenLastCalledWith(
      expect.objectContaining({
        from: "2024-06-01T00:00:00Z",
        to: "2024-06-02T00:00:00Z",
        repo: [],
      })
    );

    fireEvent.click(screen.getByRole("button", { name: "Repository" }));
    fireEvent.click(
      within(screen.getByRole("listbox", { name: "Repository options" })).getByRole("option", {
        name: /^acme\/widgets/,
      })
    );
    await waitFor(() =>
      expect(getDeliveryMeasures).toHaveBeenLastCalledWith(
        expect.objectContaining({ repo: ["acme/widgets"] })
      )
    );
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("a brush window narrows the measures to it, leaves the timeline's read on the whole window, and clearing it restores the panel's", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockResolvedValue(emptyTimeline);
  const brushStart = "2024-06-01T06:00:00.000Z";
  const brushEnd = "2024-06-01T12:00:00.000Z";
  try {
    renderPage(
      `${DELIVERY_URL}&ws=${encodeURIComponent(brushStart)}&we=${encodeURIComponent(brushEnd)}`
    );
    await screen.findByLabelText("KPI targets");
    await waitFor(() =>
      expect(getDeliveryMeasures).toHaveBeenLastCalledWith(
        expect.objectContaining({ from: brushStart, to: brushEnd })
      )
    );
    for (const [options] of getDeliveryTimeline.mock.calls) {
      expect(options).toEqual(
        expect.objectContaining({ from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" })
      );
    }

    fireEvent.click(await screen.findByRole("button", { name: "clear brush window" }));
    await waitFor(() =>
      expect(getDeliveryMeasures).toHaveBeenLastCalledWith(
        expect.objectContaining({ from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" })
      )
    );
    expect(getDeliveryTimeline).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: "2024-06-01T00:00:00Z", to: "2024-06-02T00:00:00Z" })
    );
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

test("a measures failure keeps the timeline and offers its own Retry", async () => {
  const getDeliveryTimeline = spyOn(api, "getDeliveryTimeline").mockResolvedValue(emptyTimeline);
  getDeliveryMeasures
    .mockRejectedValueOnce(new ApiError(503, { code: "INTERNAL", error: "database unavailable" }))
    .mockResolvedValue(emptyMeasures);
  try {
    renderPage();
    expect(await screen.findByText("Couldn't load the delivery measures.")).toBeDefined();
    expect(screen.getByText("No PRs in the current filter/window.")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByLabelText("KPI targets")).toBeDefined();
    expect(screen.queryByText("Couldn't load the delivery measures.")).toBeNull();
  } finally {
    cleanup();
    getDeliveryTimeline.mockRestore();
  }
});

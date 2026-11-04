import { afterEach, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { api } from "../../api/client";
import type { LegionMetricsResponse, LegionSetting } from "../../api/types";
import { LegionSection } from "./LegionSection";

/** Every spy a test installed, put back when it ends. */
const spies: { mockRestore(): void }[] = [];
afterEach(() => {
  cleanup();
  for (const spy of spies.splice(0)) spy.mockRestore();
});

const WINDOW = { from: "2026-10-06T00:00:00.000Z", to: "2026-11-03T00:00:00.000Z" };

function setting(project: string): LegionSetting {
  return {
    created_by: { kind: "user", id: "alice" },
    project,
    state_url: `http://legion-${project}.internal:13370`,
    updated_at: "2026-11-01T00:00:00Z",
  };
}

/** One answer of `GET /api/v1/legion/metrics`, as the server writes it (`metrics.Report`). */
function report(overrides: Partial<LegionMetricsResponse> = {}): LegionMetricsResponse {
  return {
    computed_at: "2026-11-03T05:04:00Z",
    finished: 2,
    from: WINDOW.from,
    phase_time: {
      bars: [
        {
          hours: 6,
          issues: 1,
          median_hours_per_issue: 6,
          role: "implementer",
          segments: {
            ci_wait: 1,
            idle: 1,
            in_a_turn: 4,
            waiting_on_children: 0,
            waiting_on_person: 0,
          },
        },
        {
          hours: 2,
          issues: 1,
          median_hours_per_issue: 2,
          role: "no_role",
          segments: {
            ci_wait: 0,
            idle: 0,
            in_a_turn: 0,
            waiting_on_children: 0,
            waiting_on_person: 2,
          },
        },
      ],
      idle_share: 0.125,
    },
    project: "ACME",
    rework: {
      hours_after_first_review: 5,
      issues: [
        {
          implementing_hours_after_first_review: 4,
          key: "ACME-7",
          review_rounds: 1,
          sent_back: true,
          testing_hours_after_first_review: 1,
        },
      ],
      median_hours_after_first_review: 5,
      median_review_rounds: 1,
      sent_back_share: 0.5,
    },
    scope: "all",
    splitting: {
      children_per_root: 1,
      filed: [
        {
          at: "2026-11-01T00:00:00Z",
          class: "in_tree_at_design",
          filer: "architect",
          filer_issue: "ACME-6",
          filer_phase: "design",
          key: "ACME-7",
          title: "The finished child",
        },
      ],
      finished_roots: 1,
      median_hours_roots_with_children: 40,
      median_hours_roots_without_children: 0,
      weeks: [{ filed: { in_tree_at_design: 1 }, finished: 2, week: "2026-10-26T00:00:00Z" }],
    },
    to: WINDOW.to,
    work_time: {
      issues: [
        {
          closed_at: "2026-11-02T00:00:00Z",
          hours: 40,
          key: "ACME-6",
          merged_at: null,
          parked_hours: 2,
          priority: 1,
          queued_hours: 3,
          root: true,
          started_at: "2026-10-31T00:00:00Z",
          title: "The finished root",
        },
        {
          closed_at: "2026-11-01T00:00:00Z",
          hours: 8,
          key: "ACME-7",
          merged_at: "2026-11-01T00:00:00Z",
          parked_hours: 0,
          priority: 2,
          queued_hours: 0,
          root: false,
          started_at: "2026-10-31T12:00:00Z",
          title: "The finished child",
        },
      ],
      median_hours: 24,
      p90_hours: 36.8,
    },
    ...overrides,
  };
}

/** The section over a window the page already resolved, so the range it reads is this one. */
async function renderSection(
  settings: LegionSetting[],
  answer: (query: { project: string; scope: string }) => LegionMetricsResponse
) {
  const list = spyOn(api, "listLegionSettings").mockResolvedValue(settings);
  const metrics = spyOn(api, "getLegionMetrics").mockImplementation((query) =>
    Promise.resolve(answer(query))
  );
  spies.push(list, metrics);
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <LegionSection compare={null} onCompare={() => {}} window={{ kind: "custom", ...WINDOW }} />
    </QueryClientProvider>
  );
  return { metrics };
}

function card(metric: string): HTMLElement {
  const found = document.querySelector(`[data-legion-metric="${metric}"]`);
  if (found === null) throw new Error(`no ${metric} card`);
  return found as HTMLElement;
}

function figure(metric: string, label: string): string {
  return card(metric).querySelector(`[data-legion-figure="${label}"]`)?.textContent ?? "";
}

test("the four metrics are drawn, each through the one chart component", async () => {
  await renderSection([setting("ACME")], () => report());

  await screen.findByText("Work time to merge");
  for (const [metric, label] of [
    ["work_time", "Work time to merge"],
    ["phase_time", "Phase time by worker type"],
    ["rework", "Rework"],
    ["splitting", "Splitting and follow-ups"],
  ]) {
    expect(card(metric).querySelector(`[data-chart="${label}"]`)).not.toBeNull();
    // Each chart has the two controls every chart of a measure over a range has.
    expect(card(metric).querySelector(`select[data-chart-compare="${label}"]`)).not.toBeNull();
    expect(screen.getByRole("group", { name: `${label} view` })).toBeDefined();
  }
  expect(document.querySelectorAll("[data-legion-metric]")).toHaveLength(4);
});

test("each card's headline numbers are the answer's own", async () => {
  await renderSection([setting("ACME")], () => report());

  await screen.findByText("Work time to merge");
  expect(figure("work_time", "Median")).toBe("24.0h");
  expect(figure("work_time", "p90")).toBe("36.8h");
  expect(figure("work_time", "Issues finished")).toBe("2");
  expect(figure("phase_time", "Idle share")).toBe("12.5%");
  expect(figure("rework", "Median review rounds")).toBe("1.0");
  expect(figure("rework", "Sent back at least once")).toBe("50.0%");
  expect(figure("splitting", "Issues filed")).toBe("1");
  expect(figure("splitting", "Roots finished")).toBe("1");
});

test("a card's table holds every number its graph draws", async () => {
  await renderSection([setting("ACME")], () => report());

  await screen.findByText("Work time to merge");
  fireEvent.click(
    screen
      .getByRole("group", { name: "Phase time by worker type view" })
      .querySelector("button:last-of-type") as HTMLElement
  );
  const table = await waitFor(() => {
    const found = document.querySelector('[data-chart-table="Phase time by worker type"]');
    if (found === null) throw new Error("no table");
    return found;
  });
  // One row per worker type, named as the card draws it, with a column per segment.
  expect(table.textContent).toContain("Worker type");
  expect(table.textContent).toContain("implementer");
  expect(table.textContent).toContain("no role");
  for (const segment of ["Waiting on a person", "In a turn", "CI wait", "Idle"]) {
    expect(table.textContent).toContain(segment);
  }
});

test("the scope toggle reads the metrics again for the roots alone", async () => {
  const { metrics } = await renderSection([setting("ACME")], (query) =>
    query.scope === "roots" ? report({ finished: 1, scope: "roots" }) : report()
  );

  await screen.findByText("Work time to merge");
  expect(figure("work_time", "Issues finished")).toBe("2");

  fireEvent.click(screen.getByRole("button", { name: "Roots" }));

  await waitFor(() => expect(figure("work_time", "Issues finished")).toBe("1"));
  expect(metrics).toHaveBeenCalledWith({
    from: WINDOW.from,
    project: "ACME",
    scope: "roots",
    to: WINDOW.to,
  });
});

test("the project picker reads the project it names, and the first that has a daemon by default", async () => {
  const { metrics } = await renderSection([setting("ACME"), setting("LINUX")], (query) =>
    report({ project: query.project })
  );

  await screen.findByText("Work time to merge");
  expect(metrics).toHaveBeenCalledWith({
    from: WINDOW.from,
    project: "ACME",
    scope: "all",
    to: WINDOW.to,
  });

  fireEvent.change(screen.getByLabelText("Legion project"), { target: { value: "LINUX" } });

  await waitFor(() =>
    expect(metrics).toHaveBeenCalledWith({
      from: WINDOW.from,
      project: "LINUX",
      scope: "all",
      to: WINDOW.to,
    })
  );
});

test("a deployment where no project names a daemon says so and reads no metrics", async () => {
  const { metrics } = await renderSection([], () => report());

  await waitFor(() =>
    expect(document.querySelector("[data-legion-metrics-unconfigured]")).not.toBeNull()
  );
  expect(document.querySelectorAll("[data-legion-metric]")).toHaveLength(0);
  expect(metrics).not.toHaveBeenCalled();
});

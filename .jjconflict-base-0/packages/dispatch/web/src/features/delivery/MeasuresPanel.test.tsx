import { afterEach, expect, test } from "bun:test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { DeliveryDailyPoint, DeliveryMeasuresResponse } from "../../api/types";
import { MeasuresPanel } from "./MeasuresPanel";

afterEach(cleanup);

/** Every UTC day of the fixture window, 2026-08-30 to 2026-09-27, with the reference request's
 *  per-day counts; the last day is partial (the window ends 20:00). */
function fixtureDaily(): DeliveryDailyPoint[] {
  const counts: Record<string, Partial<DeliveryDailyPoint>> = {
    "2026-09-05": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-06": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-09": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-11": { concluded: 1, run_success_rate: 0 },
    "2026-09-13": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-14": { concluded: 1, run_success_rate: 0 },
    "2026-09-17": { deploys: 1, concluded: 1, reached_production: 1, run_success_rate: 1 },
    "2026-09-19": { cancelled: 1 },
  };
  const days: DeliveryDailyPoint[] = [];
  for (let ms = Date.UTC(2026, 7, 30); ms <= Date.UTC(2026, 8, 27); ms += 86_400_000) {
    const day = new Date(ms).toISOString().slice(0, 10);
    days.push({
      day,
      partial: day === "2026-09-27",
      deploys: 0,
      concluded: 0,
      reached_production: 0,
      cancelled: 0,
      run_success_rate: null,
      ...counts[day],
    });
  }
  return days;
}

/** GET /api/v1/delivery/measures for the fixture window, as the server answers it with no flags
 *  stored (the reference request recorded on the slice 2 pull request). */
const fixtureMeasures: DeliveryMeasuresResponse = {
  window: { from: "2026-08-30T00:00:00Z", to: "2026-09-27T20:00:00Z" },
  computed_at: "2026-10-09T04:33:22Z",
  measures: {
    deploy_frequency: {
      successful_deploys: 5,
      deploys_with_prs: 5,
      per_day: 0.17341040462427745,
      with_prs_per_day: 0.17341040462427745,
    },
    lead_time: {
      merge_to_production: { median_minutes: 510, p90_minutes: 2736, max_minutes: 3840 },
      first_commit_to_production: { median_minutes: 1380, p90_minutes: 2988, max_minutes: 4380 },
      opened_to_production: { median_minutes: 1260, p90_minutes: 2874, max_minutes: 4320 },
      opened_to_merge: { median_minutes: 330, p90_minutes: 462.00000000000006, max_minutes: 1080 },
    },
    change_failure_rate: {
      per_pr: { confirmed: 0, pending: 0, rejected: 0, reverts: 0, total: 14, rate: 0 },
      per_deploy: {
        confirmed: 0,
        pending: 0,
        rejected: 0,
        reverts: 0,
        total: 5,
        rate: 0,
        confirmed_or_pending: 0,
        upper_bound_rate: 0,
      },
    },
    time_to_restore: { median_minutes: null },
    rework_share: 0.2857142857142857,
    deploy_run_success: {
      concluded: 7,
      reached_production: 5,
      cancelled: 1,
      rate: 0.7142857142857143,
    },
    daily: fixtureDaily(),
  },
  flags_source: "none",
  unowned_p0: [{ key: "ACME-103", title: "Production deploy gate flakes" }],
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
    change_failure_rate: true,
    change_failure_rate_upper_bound: true,
    merge_to_production_median: false,
    merge_to_production: false,
    opened_to_merge: false,
    deploy_run_success: false,
    unowned_p0: false,
  },
  freshness: {
    last_event_at: null,
    last_reconcile_at: null,
    last_error: null,
    unfetchable_count: 0,
  },
};

function renderPanel(data: DeliveryMeasuresResponse | undefined) {
  render(
    <MemoryRouter>
      <MeasuresPanel data={data} />
    </MemoryRouter>
  );
}

function card(title: string): HTMLElement {
  const found = document.querySelector<HTMLElement>(`[data-kpi="${title}"]`);
  if (found === null) throw new Error(`no [data-kpi="${title}"] card`);
  return found;
}

test("the panel shows the six KPI cards, each with its badge, number and target", () => {
  renderPanel(fixtureMeasures);

  const grid = screen.getByLabelText("KPI targets");
  expect(grid.querySelectorAll("[data-kpi]")).toHaveLength(6);

  const deploys = within(card("Deploys a day"));
  expect(deploys.getByText("0.2/day")).toBeDefined();
  expect(deploys.getByText("5 deploys")).toBeDefined();
  expect(deploys.getByText("\u2717 missed")).toBeDefined();
  expect(deploys.getByText("target \u2265 20/day")).toBeDefined();
  expect(deploys.getByRole("img", { name: "per-day trend" })).toBeDefined();

  const cfr = within(card("Change failure rate"));
  expect(cfr.getByText("0.00%")).toBeDefined();
  expect(cfr.getByText("Flags arrive with flag confirmation")).toBeDefined();
  expect(cfr.queryByText(/if every pending flag is confirmed/)).toBeNull();
  expect(cfr.getByText("target < 5% per deploy")).toBeDefined();

  const merge = within(card("Merge \u2192 production"));
  expect(merge.getByText("8.5h")).toBeDefined();
  expect(merge.getByText("2.7d")).toBeDefined();
  expect(merge.getByText("target < 45m for every change")).toBeDefined();

  const opened = within(card("PR open \u2192 merge"));
  expect(opened.getByText("5.5h")).toBeDefined();
  expect(opened.getByText("7.7h")).toBeDefined();
  expect(opened.getByText("target median < 60m")).toBeDefined();

  const runs = within(card("Deploy runs reaching production"));
  expect(runs.getByText("71.4%")).toBeDefined();
  expect(runs.getByText("5 of 7 concluded")).toBeDefined();
  expect(runs.getByText("1 cancelled runs not counted")).toBeDefined();
  expect(runs.getByText("target \u2265 90%")).toBeDefined();

  const p0 = within(card("P0 issues with no owner"));
  expect(p0.getByText("1")).toBeDefined();
  const link = p0.getByRole("link", { name: "ACME-103" });
  expect(link.getAttribute("href")).toBe("/issues/ACME-103");
  expect(link.getAttribute("title")).toBe("Production deploy gate flakes");
  expect(p0.getByText(/^no claim or route, as of /)).toBeDefined();
});

test("met, missed and no-data badges follow the response's status", () => {
  renderPanel({
    ...fixtureMeasures,
    status: { ...fixtureMeasures.status, deploys_per_day: true, deploy_run_success: null },
  });
  expect(within(card("Deploys a day")).getByText("\u2713 met")).toBeDefined();
  expect(within(card("Deploy runs reaching production")).getByText("no data")).toBeDefined();
  expect(within(card("Change failure rate")).getByText("\u2713 met")).toBeDefined();
});

test("with stored flags the change failure rate card shows the upper bound and the breakdown", () => {
  renderPanel({
    ...fixtureMeasures,
    flags_source: "stored",
    measures: {
      ...fixtureMeasures.measures,
      change_failure_rate: {
        per_pr: { confirmed: 2, pending: 1, rejected: 1, reverts: 1, total: 14, rate: 2 / 14 },
        per_deploy: {
          confirmed: 2,
          pending: 0,
          rejected: 1,
          reverts: 1,
          total: 5,
          rate: 0.4,
          confirmed_or_pending: 2,
          upper_bound_rate: 0.4,
        },
      },
    },
  });
  const cfr = within(card("Change failure rate"));
  expect(cfr.getByText("40.00%", { selector: "span.text-lg" })).toBeDefined();
  expect(
    cfr.getByText(
      (_, element) =>
        element?.tagName === "DIV" &&
        element.textContent === "up to 40.00% if every pending flag is confirmed"
    )
  ).toBeDefined();
  expect(
    cfr.getByText("2 deploys with a confirmed break; none with only unreviewed flags")
  ).toBeDefined();
  expect(cfr.queryByText("Flags arrive with flag confirmation")).toBeNull();
});

test("more than six unowned P0 issues list six and name the rest", () => {
  const unowned = Array.from({ length: 8 }, (_, i) => ({
    key: `ACME-${i + 1}`,
    title: `P0 ${i + 1}`,
  }));
  renderPanel({ ...fixtureMeasures, unowned_p0: unowned });
  const p0 = within(card("P0 issues with no owner"));
  expect(p0.getByText("8")).toBeDefined();
  expect(p0.getAllByRole("link")).toHaveLength(6);
  const more = p0.getByText("+2 more");
  expect(more.getAttribute("title")).toBe("ACME-7, ACME-8");
});

test("the strip shows the five headline numbers", () => {
  renderPanel(fixtureMeasures);
  const strip = screen.getByTestId("measures-strip");
  for (const [value, label] of [
    ["0.17/day", "Deploy freq"],
    ["8.5h", "Lead time (merge\u2192prod)"],
    ["0.0%", "CFR (per PR)"],
    ["n/a", "Time to restore"],
    ["28.6%", "Rework share"],
  ]) {
    const stat = within(strip).getByText(label).parentElement;
    expect(stat?.textContent).toBe(`${value}${label}`);
  }
});

test("the definitions fold opens to the definition cards and the per-day table, newest first", () => {
  renderPanel(fixtureMeasures);
  const toggle = screen.getByRole("button", { name: "Definitions & alternatives" });
  expect(toggle.getAttribute("aria-expanded")).toBe("false");
  expect(screen.queryByText("Lead time (first commit \u2192 prod)")).toBeNull();
  expect(screen.queryByRole("table")).toBeNull();

  fireEvent.click(toggle);

  expect(toggle.getAttribute("aria-expanded")).toBe("true");
  expect(screen.getByText("Lead time (first commit \u2192 prod)")).toBeDefined();
  expect(screen.getByText("23.0h")).toBeDefined();
  const table = screen.getByRole("table");
  const rows = within(table).getAllByRole("row");
  // A header row, then every one of the window's 29 days.
  expect(rows).toHaveLength(30);
  expect(rows[1]?.textContent?.startsWith("2026-09-27 (partial)")).toBe(true);
  expect(rows[29]?.textContent?.startsWith("2026-08-30")).toBe(true);
  const sept5 = rows.find((row) => row.textContent?.startsWith("2026-09-05"));
  expect(Array.from(sept5?.querySelectorAll("td") ?? [], (cell) => cell.textContent)).toEqual([
    "2026-09-05",
    "1",
    "1",
    "1",
    "100.0%",
    "0",
  ]);
});

test("a pending read shows the six cards' frame with an ellipsis for each number", () => {
  renderPanel(undefined);
  const grid = screen.getByLabelText("KPI targets");
  const cards = grid.querySelectorAll("[data-kpi]");
  expect(cards).toHaveLength(6);
  for (const kpi of cards) {
    expect(within(kpi as HTMLElement).getAllByText("\u2026").length).toBeGreaterThan(0);
  }
});

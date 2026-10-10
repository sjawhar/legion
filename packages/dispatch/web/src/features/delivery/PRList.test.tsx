import { expect, test } from "bun:test";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { DeliveryPR } from "../../api/types";
import { OVERSCAN_ROWS, ROW_HEIGHT_PX, scrollTopToShowRow, visibleRowRange } from "./lib/prList";
import { PRList } from "./PRList";

const base: DeliveryPR = {
  additions: 1,
  author: "octocat",
  components: [],
  created_at: "2024-06-01T00:00:00Z",
  deletions: 1,
  deploy_run: null,
  deployed_at: null,
  deployed_status: "waiting",
  first_commit_at: null,
  id: "acme/widgets#1",
  issue: null,
  issue_title: null,
  merged_at: "2024-06-01T01:00:00Z",
  number: 1,
  parent_agent: null,
  partial: false,
  priority: null,
  repo: "acme/widgets",
  rework: false,
  sessions: [],
  title: "feat: widget 1",
  unfetchable_reason: null,
  url: "https://github.com/acme/widgets/pull/1",
};

/** `count` pull requests, newest merge first by number, so the default sort (merged, newest
 *  first) lists them as #1, #2, ... */
function prs(count: number): DeliveryPR[] {
  return Array.from({ length: count }, (_, i) => ({
    ...base,
    id: `acme/widgets#${i + 1}`,
    merged_at: new Date(Date.parse("2024-06-02T00:00:00Z") - (i + 1) * 60_000).toISOString(),
    number: i + 1,
    title: `feat: widget ${i + 1}`,
    url: `https://github.com/acme/widgets/pull/${i + 1}`,
  }));
}

function renderList(
  rows: DeliveryPR[],
  onSelect = (_id: string) => {},
  onSelectRun = (_id: number) => {}
) {
  return render(
    <PRList
      allRepos={["acme/widgets"]}
      colorBy="repo"
      colorScale={new Map()}
      onSelect={onSelect}
      onSelectRun={onSelectRun}
      prs={rows}
      selectedId={undefined}
    />
  );
}

/** The body rows rendered, by their 1-based row number (the header is row 1). */
function renderedRowIndexes(): number[] {
  return [...document.querySelectorAll("tbody tr[aria-rowindex]")].map((row) =>
    Number(row.getAttribute("aria-rowindex"))
  );
}

test("the window holds the rows on screen plus the overscan, clamped at both ends", () => {
  const viewport = 10 * ROW_HEIGHT_PX;
  expect(visibleRowRange(0, viewport, 1000)).toEqual({ first: 0, last: 10 + OVERSCAN_ROWS });
  // Scrolled to the very end: the last row is inside the window, and nothing past it.
  const end = (1000 - 10) * ROW_HEIGHT_PX;
  expect(visibleRowRange(end, viewport, 1000)).toEqual({ first: 990 - OVERSCAN_ROWS, last: 1000 });
  // A taller viewport after a resize widens the window by the rows it now shows.
  expect(visibleRowRange(0, 20 * ROW_HEIGHT_PX, 1000).last).toBe(20 + OVERSCAN_ROWS);
  expect(visibleRowRange(0, viewport, 5)).toEqual({ first: 0, last: 5 });
});

test("scrolling to a row moves only as far as it takes to show it below the header", () => {
  const viewport = 11 * ROW_HEIGHT_PX; // the header plus ten body rows
  expect(scrollTopToShowRow(3, 0, viewport)).toBe(0);
  expect(scrollTopToShowRow(10, 0, viewport)).toBe(ROW_HEIGHT_PX);
  expect(scrollTopToShowRow(2, 5 * ROW_HEIGHT_PX, viewport)).toBe(2 * ROW_HEIGHT_PX);
});

test("the list renders only its first window of rows, and a taller viewport renders more", () => {
  const innerHeight = window.innerHeight;
  window.innerHeight = 10 * ROW_HEIGHT_PX;
  try {
    renderList(prs(200));
    const shown = renderedRowIndexes();
    expect(shown[0]).toBe(2);
    expect(shown.length).toBe(10 + OVERSCAN_ROWS);
  } finally {
    cleanup();
    window.innerHeight = innerHeight;
  }
});

test("arrowing past the last rendered row renders and focuses the next, and Home and End jump to the ends", () => {
  const innerHeight = window.innerHeight;
  window.innerHeight = 10 * ROW_HEIGHT_PX;
  const opened: string[] = [];
  try {
    renderList(prs(200), (id) => opened.push(id));
    const lastRendered = 10 + OVERSCAN_ROWS; // 0-based index of the first row not rendered
    const rows = () => document.querySelectorAll<HTMLElement>("tbody tr[aria-rowindex]");
    expect([...rows()].filter((row) => row.tabIndex === 0).length).toBe(1);

    rows()[0]?.focus();
    for (let i = 0; i < lastRendered; i += 1) {
      fireEvent.keyDown(document.activeElement as HTMLElement, { key: "ArrowDown" });
    }
    // Row number `lastRendered + 2` (1-based, after the header) was outside the first window.
    expect(document.activeElement?.getAttribute("aria-rowindex")).toBe(String(lastRendered + 2));
    expect(document.activeElement?.textContent).toContain(`feat: widget ${lastRendered + 1}`);

    fireEvent.keyDown(document.activeElement as HTMLElement, { key: "End" });
    expect(document.activeElement?.getAttribute("aria-rowindex")).toBe("201");
    fireEvent.keyDown(document.activeElement as HTMLElement, { key: "Home" });
    expect(document.activeElement?.getAttribute("aria-rowindex")).toBe("2");

    fireEvent.keyDown(document.activeElement as HTMLElement, { key: "Enter" });
    expect(opened).toEqual(["acme/widgets#1"]);
  } finally {
    cleanup();
    window.innerHeight = innerHeight;
  }
});

test("a deployed row's 'deployed' opens its deploy run, and the row's own click opens the PR", () => {
  const opened: string[] = [];
  const runs: number[] = [];
  try {
    renderList(
      [
        {
          ...base,
          deploy_run: 500,
          deployed_at: "2024-06-01T01:40:00Z",
          deployed_status: "deployed",
        },
      ],
      (id) => opened.push(id),
      (id) => runs.push(id)
    );
    fireEvent.click(screen.getByRole("button", { name: "Open deploy run #500" }));
    expect(runs).toEqual([500]);
    expect(opened).toEqual([]);
  } finally {
    cleanup();
  }
});

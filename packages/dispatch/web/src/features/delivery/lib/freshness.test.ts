import { expect, test } from "bun:test";
import { type DeliveryFreshness, sourceFreshness } from "./freshness";

const NOW = Date.parse("2024-06-01T12:00:00Z");

const healthy: DeliveryFreshness = {
  last_error: null,
  last_event_at: "2024-06-01T11:59:30Z",
  last_reconcile_at: "2024-06-01T11:59:00Z",
  unfetchable_count: 0,
};

const byName = (freshness: DeliveryFreshness, readAtMs = NOW) =>
  Object.fromEntries(sourceFreshness(freshness, NOW, readAtMs).map((row) => [row.name, row]));

test("the six sources of the prototype, in its order and words", () => {
  const rows = sourceFreshness(healthy, NOW, NOW - 5_000);
  expect(rows.map((row) => row.text)).toEqual([
    "PRs 1 min ago",
    "Deploy runs 1 min ago",
    "PR CI 1 min ago",
    "Dispatch 5 s ago",
    "Agents 5 s ago",
    "Events 30 s ago",
  ]);
  expect(rows.some((row) => row.red)).toBe(false);
});

test("stale: the three reconciled sources go red past three missed passes (15 min)", () => {
  const rows = byName({ ...healthy, last_reconcile_at: "2024-06-01T11:40:00Z" });
  for (const name of ["prs", "runs", "ci"]) {
    expect(rows[name]?.red).toBe(true);
    expect(rows[name]?.detail).toContain("no check for over 15 min");
  }
  expect(rows.prs?.text).toBe("PRs 20 min ago");
  expect(rows.dispatch?.red).toBe(false);
});

test("failing: a named last_error shows on each reconciled source, however recent the last pass", () => {
  const rows = byName({
    ...healthy,
    last_error: "the installation lacks Actions: read on acme/widgets",
  });
  expect(rows.runs?.red).toBe(true);
  expect(rows.runs?.text).toBe(
    "Deploy runs 1 min ago: the installation lacks Actions: read on acme/widgets"
  );
  expect(rows.events?.red).toBe(false);
});

test("never checked: no pass yet is red, naming the error when the first pass failed", () => {
  expect(byName({ ...healthy, last_reconcile_at: null }).prs?.text).toBe("PRs never checked");
  expect(byName({ ...healthy, last_reconcile_at: null, last_error: "rate limited" }).ci?.text).toBe(
    "PR CI never checked: rate limited"
  );
  expect(byName({ ...healthy, last_event_at: null }).events).toMatchObject({
    red: true,
    text: "Events never received",
  });
});

test("unfetchable: a positive count adds a red row naming it, zero adds none", () => {
  expect(byName({ ...healthy, unfetchable_count: 2 }).unfetchable).toMatchObject({
    red: true,
    text: "2 pull requests can no longer be fetched from GitHub",
  });
  expect(sourceFreshness(healthy, NOW, NOW).map((row) => row.name)).not.toContain("unfetchable");
});

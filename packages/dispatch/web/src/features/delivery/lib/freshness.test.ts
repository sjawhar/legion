import { expect, test } from "bun:test";
import type { DeliveryLiveState } from "../../../api/delivery-live";
import { type DeliveryFreshness, sourceFreshness } from "./freshness";

const NOW = Date.parse("2024-06-01T12:00:00Z");

const healthy: DeliveryFreshness = {
  last_error: null,
  last_event_at: "2024-06-01T11:59:30Z",
  last_reconcile_at: "2024-06-01T11:59:00Z",
  unfetchable_count: 0,
};

const CLOSED: DeliveryLiveState = { state: "closed", since: null, changedAt: null, verdict: null };

const byName = (freshness: DeliveryFreshness, readAtMs = NOW, live = CLOSED) =>
  Object.fromEntries(sourceFreshness(freshness, NOW, readAtMs, live).map((row) => [row.name, row]));

test("the six sources of the prototype, in its order and words", () => {
  const rows = sourceFreshness(healthy, NOW, NOW - 5_000, CLOSED);
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

test("the stale boundary: 899 s since the last pass is current, 901 s is red", () => {
  const at = (seconds: number) => new Date(NOW - seconds * 1000).toISOString();
  expect(byName({ ...healthy, last_reconcile_at: at(899) }).prs?.red).toBe(false);
  expect(byName({ ...healthy, last_reconcile_at: at(901) }).prs?.red).toBe(true);
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
  expect(sourceFreshness(healthy, NOW, NOW, CLOSED).map((row) => row.name)).not.toContain(
    "unfetchable"
  );
});

const open = (changedAt: string | null, stale: readonly string[] = []): DeliveryLiveState => ({
  state: "open",
  since: null,
  changedAt,
  verdict: stale.length === 0 ? null : { freshness: healthy, stale },
});

test("live: while the socket is open the reconciled and event rows say Live with the last change's age, and the read rows are current", () => {
  const rows = sourceFreshness(healthy, NOW, NOW - 600_000, open("2024-06-01T11:56:00Z"));
  expect(rows.map((row) => row.text)).toEqual([
    "PRs Live · last change 4 min ago",
    "Deploy runs Live · last change 4 min ago",
    "PR CI Live · last change 4 min ago",
    "Dispatch 0 s ago",
    "Agents 0 s ago",
    "Events Live · last change 4 min ago",
  ]);
  expect(rows.some((row) => row.red)).toBe(false);
  // Before any change is recorded, Live alone.
  expect(byName(healthy, NOW, open(null)).prs?.text).toBe("PRs Live");
});

test("live: a source the verdict names stale, or a reconcile error, shows its reconcile age and error in red", () => {
  const failing = byName(
    { ...healthy, last_error: "rate limited" },
    NOW,
    open("2024-06-01T11:56:00Z")
  );
  expect(failing.runs).toMatchObject({ red: true, text: "Deploy runs 1 min ago: rate limited" });
  expect(failing.events?.text).toBe("Events Live · last change 4 min ago");

  const stale = byName(
    { ...healthy, last_reconcile_at: "2024-06-01T11:40:00Z" },
    NOW,
    open("2024-06-01T11:56:00Z", ["prs", "runs", "ci"])
  );
  expect(stale.prs).toMatchObject({ red: true, text: "PRs 20 min ago" });
  expect(stale.events?.red).toBe(false);

  // The verdict names a source the page's own clock does not yet find stale: it is red all the same.
  const named = byName(healthy, NOW, open("2024-06-01T11:56:00Z", ["ci"]));
  expect(named.ci).toMatchObject({ red: true, text: "PR CI 1 min ago" });
  expect(named.prs?.text).toBe("PRs Live · last change 4 min ago");
});

test("live: while reconnecting each Live row reads Reconnecting and its seconds, in red", () => {
  const rows = byName(healthy, NOW - 600_000, {
    state: "reconnecting",
    since: NOW - 12_000,
    changedAt: "2024-06-01T11:56:00Z",
    verdict: null,
  });
  for (const name of ["prs", "runs", "ci", "events"]) {
    expect(rows[name]?.red).toBe(true);
    expect(rows[name]?.text).toEndWith(" Reconnecting, 12 s");
  }
  expect(rows.prs?.text).toBe("PRs Reconnecting, 12 s");
  // The read rows were current until the socket was lost.
  expect(rows.dispatch).toMatchObject({ red: false, text: "Dispatch 12 s ago" });
});

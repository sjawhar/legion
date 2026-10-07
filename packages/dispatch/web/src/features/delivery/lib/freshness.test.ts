import { expect, test } from "bun:test";
import { sourceFreshness } from "./freshness";

const NOW = Date.parse("2024-06-01T12:00:00Z");

test("healthy: a recent reconcile pass with no error is not red", () => {
  const [reconcile] = sourceFreshness(
    {
      last_event_at: null,
      last_reconcile_at: new Date(NOW - 60_000).toISOString(),
      last_error: null,
      unfetchable_count: 0,
    },
    NOW
  );
  expect(reconcile.red).toBe(false);
  expect(reconcile.text).toBe("Reconcile 1 min ago");
});

test("stale: no pass for over three missed intervals (15 min) is red, with no last_error", () => {
  const [reconcile] = sourceFreshness(
    {
      last_event_at: null,
      last_reconcile_at: new Date(NOW - 20 * 60_000).toISOString(),
      last_error: null,
      unfetchable_count: 0,
    },
    NOW
  );
  expect(reconcile.red).toBe(true);
  expect(reconcile.text).toBe("Reconcile 20 min ago");
  expect(reconcile.detail).toContain("no pass for over 15 min");
});

test("failing: a named last_error is shown by name and wins over the age-based staleness text", () => {
  const [reconcile] = sourceFreshness(
    {
      last_event_at: null,
      last_reconcile_at: new Date(NOW - 60_000).toISOString(),
      last_error: "the installation lacks Actions: read on acme/widgets",
      unfetchable_count: 0,
    },
    NOW
  );
  expect(reconcile.red).toBe(true);
  expect(reconcile.text).toBe(
    "Reconcile failing: the installation lacks Actions: read on acme/widgets"
  );
  // Never the generic staleness wording while a named failure is present, even for a pass this
  // recent: a stale-but-healthy row and a failing one must never read the same way.
  expect(reconcile.text).not.toContain("ago");
});

test("failing with no prior success: last_reconcile_at null and a named last_error still names the error, not 'never ran'", () => {
  const [reconcile] = sourceFreshness(
    {
      last_event_at: null,
      last_reconcile_at: null,
      last_error: "rate-limited by GitHub",
      unfetchable_count: 0,
    },
    NOW
  );
  expect(reconcile.red).toBe(true);
  expect(reconcile.text).toBe("Reconcile failing: rate-limited by GitHub");
  expect(reconcile.detail).toContain("last successful pass never");
});

test("cleared after success: a previously-failing row with last_error now null reads healthy again", () => {
  const failing = sourceFreshness(
    {
      last_event_at: null,
      last_reconcile_at: new Date(NOW - 20 * 60_000).toISOString(),
      last_error: "the installation lacks Actions: read on acme/widgets",
      unfetchable_count: 0,
    },
    NOW
  )[0];
  expect(failing.red).toBe(true);
  expect(failing.text).toContain("failing");

  const recovered = sourceFreshness(
    {
      last_event_at: null,
      last_reconcile_at: new Date(NOW - 10_000).toISOString(),
      last_error: null,
      unfetchable_count: 0,
    },
    NOW
  )[0];
  expect(recovered.red).toBe(false);
  expect(recovered.text).toBe("Reconcile 10 s ago");
});

test("never ran: no last_reconcile_at and no last_error is red with the never-ran wording", () => {
  const [reconcile] = sourceFreshness(
    { last_event_at: null, last_reconcile_at: null, last_error: null, unfetchable_count: 0 },
    NOW
  );
  expect(reconcile.red).toBe(true);
  expect(reconcile.text).toBe("Reconcile never ran");
});

test("unfetchable: a positive unfetchable_count adds a third, red row naming the count", () => {
  const rows = sourceFreshness(
    {
      last_event_at: null,
      last_reconcile_at: new Date(NOW - 60_000).toISOString(),
      last_error: null,
      unfetchable_count: 3,
    },
    NOW
  );
  expect(rows).toHaveLength(3);
  const unfetchable = rows[2];
  expect(unfetchable.name).toBe("unfetchable");
  expect(unfetchable.red).toBe(true);
  expect(unfetchable.text).toBe("3 pull requests can no longer be fetched from GitHub");
});

test("unfetchable: a zero count adds no third row", () => {
  const rows = sourceFreshness(
    {
      last_event_at: null,
      last_reconcile_at: new Date(NOW - 60_000).toISOString(),
      last_error: null,
      unfetchable_count: 0,
    },
    NOW
  );
  expect(rows).toHaveLength(2);
});

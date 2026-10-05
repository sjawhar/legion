// Pure freshness rule for the page's source row. Unlike the prototype this ports from — which
// shows six independently-polled sources (PRs, deploy runs, PR CI, Dispatch, agents, events),
// each on its own 600 s re-check cadence — this slice's API exposes only the two aggregate
// timestamps `delivery_settings` carries (LEGION-567's plan, "Reconcile"): `last_reconcile_at`,
// stamped by the one reconcile pass every 5 minutes, and `last_event_at`, stamped by intake on
// each NATS envelope it processes. There is one underlying cadence here — the reconcile's 5-minute
// ticker — not six, so this file states one interval rather than inventing six matching constants.
import type { DeliveryTimelineResponse } from "../../../api/types";

export type DeliveryFreshness = DeliveryTimelineResponse["freshness"];

/** The reconcile's own cadence (LEGION-567's plan, "Reconcile": `SyncInterval = 5 * time.Minute`). */
export const RECONCILE_INTERVAL_SECONDS = 5 * 60;

/** The reconcile row is red past this many missed intervals without a pass — the same "three
 *  missed intervals" reasoning the prototype's six-source freshness rule uses, applied to the
 *  one interval that exists in this slice. */
const STALE_INTERVALS = 3;

export interface FreshnessRow {
  name: "reconcile" | "event";
  red: boolean;
  /** "Reconcile 12 s ago", or "never checked"/"never received". */
  text: string;
  /** The hover: when it last happened, and why it is red. */
  detail: string;
}

export function formatAge(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)} s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
  return `${Math.floor(seconds / 3600)} h ago`;
}

function reconcileRow(
  lastReconcileAt: string | null,
  lastError: string | null,
  nowMs: number
): FreshnessRow {
  // A named failure always wins over the age-based staleness text: last_reconcile_at not
  // advancing on a failed pass (reconcile.go's RecordReconcileError/RecordReconcileSuccess
  // contract) means the age alone can't tell "stale because nothing's happened" apart from
  // "stale because every pass has been failing the same way" -- last_error is exactly that
  // distinction, so a stale-but-healthy row must look different from a failing one.
  if (lastError !== null) {
    const last = lastReconcileAt === null ? "never" : new Date(lastReconcileAt).toLocaleString();
    return {
      name: "reconcile",
      red: true,
      text: `Reconcile failing: ${lastError}`,
      detail: `Reconcile: failing (${lastError}); last successful pass ${last}`,
    };
  }
  if (lastReconcileAt === null) {
    return {
      name: "reconcile",
      red: true,
      text: "Reconcile never ran",
      detail: "Reconcile: no pass recorded yet",
    };
  }
  const ageSeconds = Math.max(0, (nowMs - Date.parse(lastReconcileAt)) / 1000);
  const limit = STALE_INTERVALS * RECONCILE_INTERVAL_SECONDS;
  const age = formatAge(ageSeconds);
  const checked = `Reconcile: last ran ${new Date(lastReconcileAt).toLocaleString()}`;
  if (ageSeconds > limit) {
    return {
      name: "reconcile",
      red: true,
      text: `Reconcile ${age}`,
      detail: `${checked}; no pass for over ${limit / 60} min (expected every ${RECONCILE_INTERVAL_SECONDS / 60} min)`,
    };
  }
  return { name: "reconcile", red: false, text: `Reconcile ${age}`, detail: checked };
}

function eventRow(lastEventAt: string | null, nowMs: number): FreshnessRow {
  if (lastEventAt === null) {
    return {
      name: "event",
      red: true,
      text: "Events never received",
      detail: "Events: no NATS envelope processed yet",
    };
  }
  // Live events arrive irregularly (a push on merge, a workflow_run per job) rather than on a
  // fixed interval, so unlike the reconcile row above there is no "stale past N minutes" rule
  // here — the row is informational once at least one event has been seen.
  const ageSeconds = Math.max(0, (nowMs - Date.parse(lastEventAt)) / 1000);
  return {
    name: "event",
    red: false,
    text: `Last event ${formatAge(ageSeconds)}`,
    detail: `Events: last processed ${new Date(lastEventAt).toLocaleString()}`,
  };
}

export function sourceFreshness(freshness: DeliveryFreshness, nowMs: number): FreshnessRow[] {
  return [
    reconcileRow(freshness.last_reconcile_at, freshness.last_error, nowMs),
    eventRow(freshness.last_event_at, nowMs),
  ];
}

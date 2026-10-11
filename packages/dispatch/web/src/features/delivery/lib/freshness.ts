// Pure freshness rule for the page's source row, the prototype's six sources (PRs, Deploy runs,
// PR CI, Dispatch, Agents, Events) over what Dispatch records. One reconcile pass, every five
// minutes, fetches the pull requests, the deploy runs and the PR CI runs together, so those three
// share `last_reconcile_at` and `last_error`. Dispatch's issues and the agents' titles are read
// from Dispatch's own tables while the server answers, so they are as old as the answer
// (`readAtMs`). Events are the last GitHub event intake processed. No React.
import type { DeliveryTimelineResponse } from "../../../api/types";

export type DeliveryFreshness = DeliveryTimelineResponse["freshness"];

/** The reconcile's own cadence (`SyncInterval = 5 * time.Minute`). */
export const RECONCILE_INTERVAL_SECONDS = 5 * 60;

/** A reconciled source is red past this many missed intervals without a pass. */
const STALE_INTERVALS = 3;

const RECONCILED = [
  { name: "prs", label: "PRs" },
  { name: "runs", label: "Deploy runs" },
  { name: "ci", label: "PR CI" },
] as const;

const READ_LIVE = [
  { name: "dispatch", label: "Dispatch" },
  { name: "agents", label: "Agents" },
] as const;

export type SourceName =
  | (typeof RECONCILED)[number]["name"]
  | (typeof READ_LIVE)[number]["name"]
  | "events"
  | "unfetchable";

export interface FreshnessRow {
  name: SourceName;
  red: boolean;
  /** "PRs 12 s ago", plus the error when the last pass failed. */
  text: string;
  /** The hover: when it was last checked, and why it is red. */
  detail: string;
}

export function formatAge(seconds: number): string {
  if (seconds < 60) return `${Math.floor(seconds)} s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
  return `${Math.floor(seconds / 3600)} h ago`;
}

function reconciledRow(
  name: (typeof RECONCILED)[number]["name"],
  label: string,
  freshness: DeliveryFreshness,
  nowMs: number
): FreshnessRow {
  const { last_reconcile_at: at, last_error: error } = freshness;
  if (at === null) {
    if (error !== null) {
      return {
        name,
        red: true,
        text: `${label} never checked: ${error}`,
        detail: `${label}: no reconcile pass has succeeded; the last one failed: ${error}`,
      };
    }
    return {
      name,
      red: true,
      text: `${label} never checked`,
      detail: `${label}: no check recorded yet`,
    };
  }
  const ageSeconds = Math.max(0, (nowMs - Date.parse(at)) / 1000);
  const age = formatAge(ageSeconds);
  const checked = `${label}: last checked ${new Date(at).toLocaleString()} by the reconcile`;
  // A named failure always shows: last_reconcile_at does not advance on a failed pass, so the age
  // alone cannot tell "nothing happened" from "every pass has failed the same way".
  if (error !== null) {
    return {
      name,
      red: true,
      text: `${label} ${age}: ${error}`,
      detail: `${checked}; failed since: ${error}`,
    };
  }
  const limit = STALE_INTERVALS * RECONCILE_INTERVAL_SECONDS;
  if (ageSeconds > limit) {
    return {
      name,
      red: true,
      text: `${label} ${age}`,
      detail: `${checked}; no check for over ${limit / 60} min (expected every ${RECONCILE_INTERVAL_SECONDS / 60} min)`,
    };
  }
  return { name, red: false, text: `${label} ${age}`, detail: checked };
}

function eventsRow(lastEventAt: string | null, nowMs: number): FreshnessRow {
  if (lastEventAt === null) {
    return {
      name: "events",
      red: true,
      text: "Events never received",
      detail: "Events: no GitHub event processed yet",
    };
  }
  // Live events arrive irregularly (a push on merge, a workflow run per job) rather than on a
  // fixed interval, so there is no "stale past N minutes" rule here: the reconcile rows say
  // whether the data is current.
  const ageSeconds = Math.max(0, (nowMs - Date.parse(lastEventAt)) / 1000);
  return {
    name: "events",
    red: false,
    text: `Events ${formatAge(ageSeconds)}`,
    detail: `Events: last GitHub event processed ${new Date(lastEventAt).toLocaleString()}`,
  };
}

export function sourceFreshness(
  freshness: DeliveryFreshness,
  nowMs: number,
  readAtMs: number
): FreshnessRow[] {
  const readAge = formatAge(Math.max(0, (nowMs - readAtMs) / 1000));
  const rows: FreshnessRow[] = [
    ...RECONCILED.map(({ name, label }) => reconciledRow(name, label, freshness, nowMs)),
    ...READ_LIVE.map(({ name, label }) => ({
      name,
      red: false,
      text: `${label} ${readAge}`,
      detail: `${label}: read when the page was last answered, ${new Date(readAtMs).toLocaleString()}`,
    })),
    eventsRow(freshness.last_event_at, nowMs),
  ];
  // A population pull request whose completing fetch answered a permanent 404/410 from GitHub is
  // marked unfetchable rather than retried every pass forever: surfaced here, by count, apart
  // from the reconcile's own health, since it is a per-pull-request condition.
  if (freshness.unfetchable_count > 0) {
    const count = freshness.unfetchable_count;
    rows.push({
      name: "unfetchable",
      red: true,
      text: `${count} pull request${count === 1 ? "" : "s"} can no longer be fetched from GitHub`,
      detail:
        "A 404 or 410 from GitHub for this pull request (or its repository) -- see the drill-down for which one and why.",
    });
  }
  return rows;
}

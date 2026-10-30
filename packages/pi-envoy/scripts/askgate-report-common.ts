// What both AskGate reports read and count with: the JSON field readers a session entry and a
// decision record are read through, and the four counts they share. `askgate-report.ts` reports
// over the session files on disk; `askgate-report-dispatch.ts` over a Dispatch project's decision
// documents, and the session report imports it for the Dispatch half of its own, so a count both
// of them make lives here rather than in either report.

/** A value and how many entries hold it. */
export interface Count {
  name: string;
  count: number;
}

export type Json = Record<string, unknown>;

export function isObject(value: unknown): value is Json {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function str(value: unknown): string {
  return typeof value === "string" ? value : "";
}

export function strOrNull(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

export function num(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

/** Nearest-rank percentile; null when there are no values. */
export function percentile(values: number[], p: number): number | null {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  const rank = Math.max(1, Math.ceil((p / 100) * sorted.length));
  return sorted[rank - 1] ?? null;
}

/**
 * Counts each value. The names in `first` come first, in that order, even at zero; every other
 * value follows, most first, then by name.
 */
export function tally(values: string[], first: readonly string[] = []): Count[] {
  const counts = new Map<string, number>(first.map((name) => [name, 0]));
  for (const value of values) counts.set(value, (counts.get(value) ?? 0) + 1);
  const rest = [...counts]
    .filter(([name]) => !first.includes(name))
    .sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  return [
    ...first.map((name) => ({ name, count: counts.get(name) ?? 0 })),
    ...rest.map(([name, count]) => ({ name, count })),
  ];
}

/** What the override walk reads: a gate entry and a decision record alike. */
export interface Overridable {
  session: string;
  target: string | null;
  rule: string | null;
  outcome: string;
}

/**
 * The rules refusals (records with a rule) that the agent overrode, each with what overrode it:
 * in each session, in time order, the next record with the refusal's target is a rebuttal or a
 * breaker. The time is read through `stamp`, since a gate entry carries it as `timestamp` and a
 * decision record as `time`.
 */
export function overrides<T extends Overridable>(
  records: readonly T[],
  stamp: (record: T) => string
): { refusal: T; by: "rebuttal" | "breaker" }[] {
  const found: { refusal: T; by: "rebuttal" | "breaker" }[] = [];
  const sessions = new Map<string, T[]>();
  for (const record of records) {
    const list = sessions.get(record.session) ?? [];
    list.push(record);
    sessions.set(record.session, list);
  }
  for (const list of sessions.values()) {
    // A stable sort by time: entries stamped in the same millisecond keep their file order.
    const ordered = list
      .map((record, index) => {
        const at = Date.parse(stamp(record));
        return { record, index, at: Number.isFinite(at) ? at : Number.NEGATIVE_INFINITY };
      })
      .sort((a, b) => a.at - b.at || a.index - b.index);
    // Walk back from the end, so each target's next outcome is known when its refusal is reached.
    const next = new Map<string, string>();
    for (let i = ordered.length - 1; i >= 0; i--) {
      const { record } = ordered[i] as { record: T };
      if (record.target === null) continue;
      if (record.rule !== null) {
        const after = next.get(record.target);
        if (after === "rebuttal" || after === "breaker") found.push({ refusal: record, by: after });
      }
      next.set(record.target, record.outcome);
    }
  }
  return found;
}

/** One count as a share of another, to a tenth of a percent; `n/a` when there is nothing to divide. */
export function share(part: number, whole: number): string {
  return whole === 0 ? "n/a" : `${((part / whole) * 100).toFixed(1)}%`;
}

import { type ReactNode, useEffect, useState } from "react";

import { useTooltips } from "../../components/tooltips";
import { authorText } from "../issue/page-structure";

const relativeFormatter = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

const RELATIVE_UNITS: Array<{ ms: number; unit: Intl.RelativeTimeFormatUnit }> = [
  { ms: 365 * 24 * 60 * 60 * 1000, unit: "year" },
  { ms: 30 * 24 * 60 * 60 * 1000, unit: "month" },
  { ms: 7 * 24 * 60 * 60 * 1000, unit: "week" },
  { ms: 24 * 60 * 60 * 1000, unit: "day" },
  { ms: 60 * 60 * 1000, unit: "hour" },
  { ms: 60 * 1000, unit: "minute" },
];

const TICK_INTERVAL_MS = 30_000;

// Every mounted <Timestamp> subscribes to one shared tick instead of running its own
// setInterval, so a page with hundreds of timestamps (a long log, a comment thread) still
// pays for exactly one timer.
const listeners = new Set<() => void>();
let scheduled = false;

function scheduleTick(): void {
  scheduled = true;
  setTimeout(() => {
    if (listeners.size === 0) {
      scheduled = false;
      return;
    }
    for (const listener of listeners) {
      listener();
    }
    scheduleTick();
  }, TICK_INTERVAL_MS);
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (!scheduled) {
    scheduleTick();
  }
  return () => {
    listeners.delete(listener);
  };
}

/** "4 minutes ago", "just now": the relative form every `<Timestamp>` shows, for a caller that
 *  needs it as text (an attribute). */
export function relativeTimeLabel(at: Date, now: number): string | undefined {
  const deltaMs = at.getTime() - now;
  if (Math.abs(deltaMs) < 60_000) {
    return "just now";
  }
  for (const { ms, unit } of RELATIVE_UNITS) {
    if (Math.abs(deltaMs) >= ms) {
      return relativeFormatter.format(Math.round(deltaMs / ms), unit);
    }
  }
}

export interface TimestampProps {
  at: string;
  className?: string;
  /** Draws the instant itself after the relative age on a surface that raises no tooltip, for
   *  one where the clock time is what a reader weighs (a claim's age decides whether it is
   *  stale). Where a tooltip carries the instant it is not drawn as well: the two say the same
   *  thing, and the drawn one would widen a chip a narrow host - a Board card, a List row -
   *  has no room for. */
  exact?: boolean;
}

/**
 * A relative time ("4 minutes ago") that keeps itself current, with the instant itself in
 * `dateTime`, on hover (`title`) and, where a caller asks for it, drawn as text beside it.
 *
 * On a surface that offers no tooltips (`components/tooltips.tsx`, the issue page) it carries
 * no `title`, and a time whose exact instant a reader needs says it in words instead: the
 * issue's claim is the one that does (`ClaimChip.tsx`), because how long a session has held
 * the issue is what decides whether its claim is stale.
 */
export function Timestamp({ at, className, exact = false }: TimestampProps): ReactNode {
  const tooltips = useTooltips();
  const [, forceUpdate] = useState(0);
  useEffect(() => subscribe(() => forceUpdate((count) => count + 1)), []);

  const date = new Date(at);
  if (Number.isNaN(date.getTime())) {
    return (
      <time className={className} {...authorText}>
        {at}
      </time>
    );
  }
  return (
    <time
      className={className}
      dateTime={at}
      title={tooltips ? date.toLocaleString() : undefined}
      {...authorText}
    >
      {relativeTimeLabel(date, Date.now())}
      {exact && !tooltips
        ? ` (${date.toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" })})`
        : null}
    </time>
  );
}

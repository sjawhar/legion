import { type ReactNode, useEffect, useState } from "react";

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

function relativeTimeLabel(at: Date, now: number) {
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
}

/** Renders a relative time ("4 minutes ago") that keeps itself current, with the absolute
 * value available on hover (`title`) and to assistive tech (`datetime`). */
export function Timestamp({ at, className }: TimestampProps): ReactNode {
  const [, forceUpdate] = useState(0);
  useEffect(() => subscribe(() => forceUpdate((count) => count + 1)), []);

  const date = new Date(at);
  if (Number.isNaN(date.getTime())) {
    return <time className={className}>{at}</time>;
  }
  return (
    <time className={className} dateTime={at} title={date.toLocaleString()}>
      {relativeTimeLabel(date, Date.now())}
    </time>
  );
}

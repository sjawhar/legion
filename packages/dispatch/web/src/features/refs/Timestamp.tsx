import { type ReactNode, useEffect, useState } from "react";

interface TimestampProps {
  at: string;
}

const relativeUnits: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 1000 * 60 * 60 * 24 * 365],
  ["month", 1000 * 60 * 60 * 24 * 30],
  ["week", 1000 * 60 * 60 * 24 * 7],
  ["day", 1000 * 60 * 60 * 24],
  ["hour", 1000 * 60 * 60],
  ["minute", 1000 * 60],
];

const relativeTimeFormat = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

function relativeLabel(at: string): string {
  const elapsedMs = new Date(at).getTime() - Date.now();
  for (const [unit, unitMs] of relativeUnits) {
    if (Math.abs(elapsedMs) >= unitMs) {
      return relativeTimeFormat.format(Math.round(elapsedMs / unitMs), unit);
    }
  }
  return relativeTimeFormat.format(Math.round(elapsedMs / 1000), "second");
}

/** Renders a self-updating relative time ("4 minutes ago"), with the absolute time in a
 *  hover `title` and machine-readable in `dateTime` for assistive tech. */
export function Timestamp({ at }: TimestampProps): ReactNode {
  const [, retick] = useState(0);
  useEffect(() => {
    const interval = setInterval(() => retick((tick) => tick + 1), 60_000);
    return () => clearInterval(interval);
  }, []);

  return (
    <time dateTime={at} title={new Date(at).toLocaleString()}>
      {relativeLabel(at)}
    </time>
  );
}

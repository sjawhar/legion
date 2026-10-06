import { type ReactNode, useEffect, useState } from "react";

import {
  connectionDotConnected,
  connectionDotFailed,
  dangerText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { type DeliveryFreshness, sourceFreshness } from "./lib/freshness";

/** One row per freshness fact sourceFreshness reports: the two aggregate timestamps (reconcile,
 *  events) and, when positive, the unfetchable pull-request count -- red, with its reason in the
 *  text and on hover, when it has gone stale, never happened, or (the count) is nonzero. Ticks
 *  every second on its own, so the ages move between refetches without re-rendering the page. */
export function SourceFreshness({ freshness }: { freshness: DeliveryFreshness }): ReactNode {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  return (
    <div
      aria-label="Source freshness"
      className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs"
      role="status"
    >
      {sourceFreshness(freshness, now).map((row) => (
        <span
          key={row.name}
          title={row.detail}
          data-source={row.name}
          data-red={row.red}
          className={`flex items-center gap-1 ${row.red ? dangerText : textMutedOnCanvas}`}
        >
          <span
            aria-hidden="true"
            className={`h-1.5 w-1.5 shrink-0 rounded-full ${row.red ? connectionDotFailed : connectionDotConnected}`}
          />
          <span>{row.text}</span>
        </span>
      ))}
    </div>
  );
}

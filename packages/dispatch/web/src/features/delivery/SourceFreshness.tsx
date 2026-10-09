import { type ReactNode, useEffect, useState } from "react";

import {
  connectionDotConnected,
  connectionDotFailed,
  dangerText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { type DeliveryFreshness, sourceFreshness } from "./lib/freshness";

/** One row naming each source with how long ago it was checked (sourceFreshness): red, with its
 *  last error in the text and on hover, when its last check failed or it has gone stale. `readAtMs`
 *  is when the timeline was last answered, which is when Dispatch's issues and the agents' titles
 *  were read. Ticks every second on its own, so the ages move between refetches without
 *  re-rendering the page. */
export function SourceFreshness({
  freshness,
  readAtMs,
}: {
  freshness: DeliveryFreshness;
  readAtMs: number;
}): ReactNode {
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
      {sourceFreshness(freshness, now, readAtMs).map((row) => (
        <span
          className={`flex max-w-[36rem] items-center gap-1 ${row.red ? dangerText : textMutedOnCanvas}`}
          data-red={row.red}
          data-source={row.name}
          key={row.name}
          title={row.detail}
        >
          <span
            aria-hidden="true"
            className={`h-1.5 w-1.5 shrink-0 rounded-full ${row.red ? connectionDotFailed : connectionDotConnected}`}
          />
          <span className="truncate">{row.text}</span>
        </span>
      ))}
    </div>
  );
}

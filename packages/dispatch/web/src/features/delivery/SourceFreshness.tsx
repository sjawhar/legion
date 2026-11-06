import { type ReactNode, useEffect, useState } from "react";

import { useLiveSocketState } from "../../api/live-socket";
import {
  connectionDotConnected,
  connectionDotFailed,
  dangerText,
  textMutedOnCanvas,
} from "../../theme/classes";
import { type DeliveryFreshness, sourceFreshness } from "./lib/freshness";

/** One row naming each source with how long ago it was checked (sourceFreshness): red, with its
 *  last error in the text and on hover, when its last check failed or it has gone stale. While the
 *  page's live socket is open, the reconciled and event rows say Live instead, aged by the last
 *  change the page applied, and read Reconnecting in red while the socket is lost. `readAtMs` is
 *  when the page was last answered, which is when Dispatch's issues and the agents' titles were
 *  read. Ticks every second on its own, so the ages move without re-rendering the page. The ticking
 *  ages are not a live region: a screen reader hears the visually hidden status line instead,
 *  which changes only when a source turns red or recovers. */
export function SourceFreshness({
  freshness,
  readAtMs,
}: {
  freshness: DeliveryFreshness;
  readAtMs: number;
}): ReactNode {
  const live = useLiveSocketState();
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  const rows = sourceFreshness(freshness, now, readAtMs, live);
  const red = rows.filter((row) => row.red);
  const announcement =
    red.length === 0
      ? "Every source is current."
      : `Not current: ${red.map((row) => row.detail).join("; ")}`;

  return (
    <section aria-label="Source freshness">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
        {rows.map((row) => (
          // `min-w-0`: the row's text is one `truncate` line (`white-space: nowrap`), so its
          // min-content width is the whole sentence, and a flex item's automatic minimum size is
          // that width. Without this, a row naming a reconcile error stands at its full length
          // past the column, and a phone, whose layout follows its widest content, widens and
          // scrolls the whole page under the reader rather than clipping it (INFRA-1260).
          <span
            className={`flex max-w-[36rem] min-w-0 items-center gap-1 ${row.red ? dangerText : textMutedOnCanvas}`}
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
      <p className="sr-only" role="status">
        {announcement}
      </p>
    </section>
  );
}

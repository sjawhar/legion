import type { ReactNode } from "react";

import { useLiveSocketPill } from "../../api/live-socket";
import {
  badgeBlocking,
  calloutDangerBg,
  calloutDangerBorder,
  calloutWarningBorder,
  statusConnecting,
} from "../../theme/classes";
import { AfterTheFirstBatch } from "../doc/AfterTheFirstBatch";

/**
 * Whether the page's live updates are reaching it, in the shell's status corner.
 *
 * It says nothing while the socket is up, which is every second of a load that goes well, and
 * what it would say is nothing a reader who opened a document is waiting for - so on a document
 * address it is drawn after the editor's first batch, along with the rest of the shell's own
 * parts (dispatch://INFRA-341 R11, `features/doc/AfterTheFirstBatch.tsx`). It stands in a corner
 * fixed over the page and takes no space in it, so nothing moves whenever it arrives, and its
 * space is what it draws itself until the socket has something to report: nothing.
 *
 * Its own component, so the state it watches re-renders this pill rather than the whole shell.
 */
export function ConnectionPill(): ReactNode {
  return (
    <AfterTheFirstBatch space={null}>
      <WhatTheSocketSays />
    </AfterTheFirstBatch>
  );
}

function WhatTheSocketSays(): ReactNode {
  const connection = useLiveSocketPill();
  // A socket Dispatch refuses carries no status the page can read, so nothing here is terminal:
  // once reconnecting has gone on for two minutes it offers a reload, and the page keeps trying
  // underneath.
  if (connection === "stuck") {
    return (
      <p
        aria-live="polite"
        className={`flex items-center gap-2 rounded-full border px-3 py-1 text-xs font-medium shadow-lg ${calloutDangerBorder} ${calloutDangerBg} ${badgeBlocking.text}`}
        data-testid="connection-pill"
      >
        Live updates are not reaching this page
        <button className="underline" onClick={() => window.location.reload()} type="button">
          Reload
        </button>
      </p>
    );
  }
  if (connection === "reconnecting") {
    return (
      <p
        aria-live="polite"
        className={`rounded-full border px-3 py-1 text-xs font-medium shadow-lg ${calloutWarningBorder} ${statusConnecting.bg} ${statusConnecting.text}`}
        data-testid="connection-pill"
      >
        Reconnecting…
      </p>
    );
  }
  return null;
}

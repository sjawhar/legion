import type { ReactNode } from "react";

import {
  badgeLow,
  hoverToDangerText,
  liveDotBg,
  offlineDotBg,
  secondaryButtonText,
  unknownDotBorder,
} from "../../theme/classes";

/** The pill a session is listed in (subscribed agents, an ask's followers). */
export const sessionChipClassName = `flex items-center gap-2 rounded-full px-3 py-1 text-sm ${badgeLow.bg} ${badgeLow.text}`;

/** The chip's remove control, red on hover. */
export const sessionRemoveButtonClassName = `font-medium ${secondaryButtonText} ${hoverToDangerText} disabled:cursor-not-allowed disabled:opacity-50`;

const LIVENESS = {
  live: { dot: liveDotBg, title: "Live" },
  offline: { dot: offlineDotBg, title: "Not live" },
  unknown: { dot: unknownDotBorder, title: "Liveness unknown" },
} as const;

/** The liveness dot at the head of a session chip: green while Envoy hears the session, and a
 *  hollow ring when `live` is `undefined` - the agent list it is judged against is refilling or
 *  unreachable, which says nothing of whether the session is gone. */
export function LiveDot({ live }: { live: boolean | undefined }): ReactNode {
  const state = live === undefined ? "unknown" : live ? "live" : "offline";
  return (
    <span
      aria-hidden="true"
      className={`h-2 w-2 shrink-0 rounded-full ${LIVENESS[state].dot}`}
      data-live={state}
      title={LIVENESS[state].title}
    />
  );
}

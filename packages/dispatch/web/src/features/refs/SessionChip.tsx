import type { ReactNode } from "react";

import {
  badgeLow,
  hoverToDangerText,
  liveDotBg,
  offlineDotBg,
  secondaryButtonText,
} from "../../theme/classes";

/** The pill a session is listed in (subscribed agents, an ask's followers). */
export const sessionChipClassName = `flex items-center gap-2 rounded-full px-3 py-1 text-sm ${badgeLow.bg} ${badgeLow.text}`;

/** The chip's remove control, red on hover. */
export const sessionRemoveButtonClassName = `font-medium ${secondaryButtonText} ${hoverToDangerText} disabled:cursor-not-allowed disabled:opacity-50`;

/** The liveness dot at the head of a session chip: green while Envoy hears the session. */
export function LiveDot({ live }: { live: boolean }): ReactNode {
  return (
    <span
      aria-hidden="true"
      className={`h-2 w-2 shrink-0 rounded-full ${live ? liveDotBg : offlineDotBg}`}
      title={live ? "Live" : "Not live"}
    />
  );
}

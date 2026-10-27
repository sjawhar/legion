import type { ReactNode } from "react";

import type { Actor } from "../../api/types";
import { surfaceMutedStrongBg, textSecondaryOnSurface } from "../../theme/classes";
import { useAgentText } from "../refs/AgentName";

/** A turn's author badge: the initials of the author's name (`useAgentText().initials`: the
 *  name alone, never its `(for <owner>)` attribution) in a round (human) or square (session)
 *  tile. */
export function Avatar({ actor }: { actor: Actor }): ReactNode {
  const text = useAgentText();
  return (
    <span
      aria-hidden="true"
      className={`grid h-8 w-8 shrink-0 place-items-center text-xs font-semibold ${
        actor.kind === "session" ? "rounded-md" : "rounded-full"
      } ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`}
    >
      {text.initials(actor)}
    </span>
  );
}

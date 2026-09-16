import type { ReactNode } from "react";

import { surfaceMutedStrongBg, textSecondaryOnSurface } from "../../theme/classes";
import type { Author } from "./authors";

/** A turn's author badge: initials in a round (human) or square (session) tile. */
export function Avatar({ author }: { author: Author }): ReactNode {
  return (
    <span
      aria-hidden="true"
      className={`grid h-8 w-8 shrink-0 place-items-center text-xs font-semibold ${
        author.shape === "round" ? "rounded-full" : "rounded-md"
      } ${surfaceMutedStrongBg} ${textSecondaryOnSurface}`}
    >
      {author.initials}
    </span>
  );
}

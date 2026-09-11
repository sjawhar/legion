import type { ReactNode } from "react";

import type { Event } from "../../api/types";
import { borderDefault, textMutedOnSurface } from "../../theme/classes";
import { EventBody, isAskEvent } from "../issue/EventBody";
import { actorLabel } from "../refs/actor";

interface PinnedTabProps {
  events: Event[];
  issueKey: string | undefined;
  pinnedIds: string[];
}

export function PinnedTab({ events, pinnedIds }: PinnedTabProps): ReactNode {
  return (
    <div className="pt-3">
      {events.map((event) => (
        <article className={`rounded-lg border p-3 text-sm ${borderDefault}`} key={event.id}>
          <EventBody event={event} />
          {isAskEvent(event) ? null : (
            <p className={`mt-1 text-xs ${textMutedOnSurface}`}>{actorLabel(event.actor)}</p>
          )}
        </article>
      ))}
      {pinnedIds.length === 0 ? (
        <p className={`text-sm ${textMutedOnSurface}`}>No pinned items.</p>
      ) : null}
    </div>
  );
}

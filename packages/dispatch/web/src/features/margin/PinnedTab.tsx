import type { ReactNode } from "react";

import type { Event } from "../../api/types";
import { actorLabel } from "../refs/actor";

interface PinnedTabProps {
  events: Event[];
  pinnedIds: string[];
}

export function PinnedTab({ events, pinnedIds }: PinnedTabProps): ReactNode {
  return (
    <div className="max-h-[45dvh] overflow-y-auto pt-3">
      {events.map((event) => (
        <article className="rounded-lg border border-slate-200 p-3 text-sm" key={event.id}>
          <p className="font-medium text-slate-900">{event.type.replace(".", " ")}</p>
          <p className="mt-1 text-xs text-slate-500">{actorLabel(event.actor)}</p>
        </article>
      ))}
      {pinnedIds.length === 0 ? <p className="text-sm text-slate-500">No pinned items.</p> : null}
    </div>
  );
}

import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { Issue, UserIssueState } from "../../api/types";
import { AskCard } from "../inbox/AskCard";
import { pinnedLogItems } from "./LogTab";

export function BoardStrip({ issue, state }: { issue: Issue; state: UserIssueState }): ReactNode {
  const inbox = useQuery({ queryKey: ["inbox"], queryFn: () => api.getInbox() });
  const events = useQuery({
    queryKey: ["events", issue.key, "board"],
    queryFn: () => api.getIssueEvents(issue.key, { limit: 200 }),
  });
  const openAsks = (inbox.data ?? []).filter((ask) => ask.issue_key === issue.key);
  const pinned = pinnedLogItems(events.data ?? [], state);

  if (openAsks.length === 0 && pinned.length === 0) {
    return null;
  }

  return (
    <section
      aria-label="Issue board"
      className="mb-6 space-y-4 rounded-xl border border-slate-200 bg-slate-100 p-4"
    >
      {pinned.length === 0 ? null : (
        <div>
          <h2 className="text-sm font-semibold text-slate-800">Pinned items</h2>
          <ul className="mt-2 space-y-1 text-sm text-slate-600">
            {pinned.map((event) => (
              <li key={event.id}>Event {event.seq}</li>
            ))}
          </ul>
        </div>
      )}
      {openAsks.length === 0 ? null : (
        <div className="space-y-3">
          <h2 className="text-sm font-semibold text-slate-800">Open asks</h2>
          {openAsks.map((ask) => (
            <AskCard ask={ask} key={ask.id} />
          ))}
        </div>
      )}
    </section>
  );
}

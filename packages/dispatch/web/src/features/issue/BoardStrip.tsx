import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type { Event, Issue, UserIssueState } from "../../api/types";
import { AskCard } from "../inbox/AskCard";
import { pinnedEventIds } from "./log-model";

const pinnedEventBatchSize = 50;

type IssueEventsFetcher = (issueKey: string, options: { ids: string[] }) => Promise<Event[]>;

export async function fetchPinnedEvents(
  listIssueEvents: IssueEventsFetcher,
  issueKey: string,
  ids: string[]
): Promise<Event[]> {
  const batches: string[][] = [];
  for (let index = 0; index < ids.length; index += pinnedEventBatchSize) {
    batches.push(ids.slice(index, index + pinnedEventBatchSize));
  }
  const events = await Promise.all(
    batches.map((batch) => listIssueEvents(issueKey, { ids: batch }))
  );
  return events.flat().sort((left, right) => left.seq - right.seq);
}

export function BoardStrip({ issue, state }: { issue: Issue; state: UserIssueState }): ReactNode {
  const inbox = useQuery({ queryKey: ["inbox"], queryFn: () => api.getInbox() });
  const ids = pinnedEventIds(state.dismissed);
  const events = useQuery({
    enabled: ids.length > 0,
    queryKey: ["events", issue.key, "board", ids],
    queryFn: () => fetchPinnedEvents(api.getIssueEvents.bind(api), issue.key, ids),
  });
  const openAsks = (inbox.data ?? []).filter((ask) => ask.issue_key === issue.key);
  const pinned = events.data ?? [];

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

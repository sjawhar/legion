import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { userAgentStateQuery } from "../../api/queries";
import type { Agent } from "../../api/types";
import { LabelPill } from "../../components/Pill";
import { borderDefault, card, textMutedOnCanvas } from "../../theme/classes";
import { AgentName } from "../refs/AgentName";

import { unreadRepliesLabel } from "./unread";

/**
 * Sessions that answered the viewer and are no longer in the live list. The unread count sums
 * every session that replied, and a session often answers and then exits, so each of them keeps
 * a row here whose name links to its live view, where its replies are read: the badge is always
 * one the viewer can clear, and a reply is never lost because its session ended.
 */
export function EndedAgentsWithReplies({ live }: { live: readonly Agent[] }): ReactNode {
  const states = useQuery(userAgentStateQuery()).data ?? {};
  const ended = Object.entries(states)
    .map(([sessionID, state]) => [sessionID, state.unread_replies ?? 0] as const)
    .filter(
      ([sessionID, unread]) => unread > 0 && !live.some((agent) => agent.session_id === sessionID)
    );
  if (ended.length === 0) return null;
  const title = "Replied, no longer connected";
  return (
    <section aria-label={title} className="mt-5 space-y-3">
      <h2 className={`text-xs font-semibold uppercase ${textMutedOnCanvas}`}>{title}</h2>
      {ended.map(([sessionID, unread]) => (
        <article
          className={`flex flex-wrap items-center gap-x-3 gap-y-1 rounded-xl border px-3 py-1.5 ${card} ${borderDefault}`}
          key={sessionID}
        >
          <AgentName
            className="min-w-0 truncate text-sm font-semibold"
            sessionId={sessionID}
            storedTitle={undefined}
          />
          <span className="ml-auto">
            <LabelPill selected>{unreadRepliesLabel(unread)}</LabelPill>
          </span>
        </article>
      ))}
    </section>
  );
}

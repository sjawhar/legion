import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";

import { api } from "../../api/client";
import { queryKeys } from "../../api/query-keys";
import type { AskFollower } from "../../api/types";
import { QueryError } from "../../components/QueryError";
import {
  badgeLow,
  hoverToDangerText,
  liveDotBg,
  offlineDotBg,
  secondaryButtonText,
  textMutedOnSurface,
} from "../../theme/classes";
import { useAgents } from "../conversation/useAgents";
import { UnsubscribeDialog } from "../issue/UnsubscribeDialog";
import { sessionLabel } from "../refs/actor";

const removeAskFollower = (askId: string, sessionId: string): Promise<void> =>
  api.removeAskFollower(askId, sessionId);

export interface AskFollowersProps {
  askId: string;
  /** Oldest first, as `GET /api/v1/asks/{id}` returns them. */
  followers: readonly AskFollower[];
  /** Write seam for tests; defaults to the real API. */
  removeFollower?: (askId: string, sessionId: string) => Promise<void>;
}

/**
 * The sessions an ask's answer and replies reach directly: every session that wrote to
 * the ask, minus any that unfollowed. Each shows a live/offline dot and its title (or
 * shortened session id) from the agent registry, and an Unfollow control that confirms
 * before removing the session, which is then told.
 */
export function AskFollowers({
  askId,
  followers,
  removeFollower = removeAskFollower,
}: AskFollowersProps): ReactNode {
  const queryClient = useQueryClient();
  const { agents } = useAgents(followers.length > 0);
  const [confirming, setConfirming] = useState<AskFollower | undefined>(undefined);
  const unfollow = useMutation({
    mutationFn: (sessionId: string) => removeFollower(askId, sessionId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: queryKeys.askThread(askId) }),
  });

  if (followers.length === 0) {
    return null;
  }

  const labelFor = (sessionId: string): string =>
    sessionLabel(sessionId, agents.find((agent) => agent.session_id === sessionId)?.title);

  return (
    <section aria-label="Followers" className="mt-2">
      <h3 className={`text-xs font-semibold ${textMutedOnSurface}`}>
        Followed by {followers.length}
      </h3>
      <ul className="mt-1 flex flex-wrap gap-2">
        {followers.map((follower) => {
          const live = agents.some((agent) => agent.session_id === follower.session_id);
          return (
            <li
              className={`flex items-center gap-2 rounded-full px-3 py-1 text-sm ${badgeLow.bg} ${badgeLow.text}`}
              key={follower.session_id}
            >
              <span
                aria-hidden="true"
                className={`h-2 w-2 shrink-0 rounded-full ${live ? liveDotBg : offlineDotBg}`}
                title={live ? "Live" : "Not live"}
              />
              <span title={follower.session_id}>{labelFor(follower.session_id)}</span>
              <button
                className={`font-medium ${secondaryButtonText} ${hoverToDangerText} disabled:cursor-not-allowed disabled:opacity-50`}
                disabled={unfollow.isPending}
                onClick={() => setConfirming(follower)}
                type="button"
              >
                Unfollow
              </button>
            </li>
          );
        })}
      </ul>
      {unfollow.isError ? (
        <div className="mt-2">
          <QueryError
            message="Could not unfollow."
            onRetry={() => unfollow.mutate(unfollow.variables)}
            retrying={unfollow.isPending}
          />
        </div>
      ) : null}
      {confirming === undefined ? null : (
        <UnsubscribeDialog
          label="Unfollow"
          message={`Unfollow ${labelFor(confirming.session_id)} from this ask? Its answer and replies will no longer reach them. They will be told.`}
          onCancel={() => setConfirming(undefined)}
          onConfirm={() => {
            unfollow.mutate(confirming.session_id);
            setConfirming(undefined);
          }}
        />
      )}
    </section>
  );
}

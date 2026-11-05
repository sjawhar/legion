import { LEGION_ROLES, type LegionRole } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { api } from "../../api/client";
import type {
  IssueCollaboratorAgent,
  IssueCollaboratorLegion,
  IssueCollaboratorPerson,
  IssueCollaborators,
} from "../../api/types";
import { EmptyState } from "../../components/EmptyState";
import { QueryError } from "../../components/QueryError";
import { compareTimestamps, isTimestamp } from "../../lib/timestamps";
import {
  borderDefault,
  card,
  textMutedOnCanvas,
  textMutedOnSurface,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { AgentName } from "../refs/AgentName";
import { Timestamp } from "../refs/Timestamp";

type LegionAgent = IssueCollaboratorAgent & { readonly legion: IssueCollaboratorLegion };

/** The tab's three sections, in the order the reader meets them. */
export interface CollaboratorSections {
  /** Legion agents by role in workflow order (`LEGION_ROLES`), each role's newest first. */
  readonly legion: readonly {
    readonly role: LegionRole;
    readonly agents: readonly LegionAgent[];
  }[];
  /** Every other agent session, the most recently active first. */
  readonly others: readonly IssueCollaboratorAgent[];
  /** People, in the server's order (newest first). */
  readonly people: readonly IssueCollaboratorPerson[];
}

/** Newer first; a missing time sorts after every time, and so does one this reader cannot order.
 *  `started_at` and `last_at` are the Legion daemon's and the listener's, relayed verbatim, so
 *  they are asked of `isTimestamp` first: `compareTimestamps` throws on anything but the server's
 *  own form, and that throw would land in a render. */
function newestFirst(left: string | null, right: string | null): number {
  const known = (value: string | null): value is string => value !== null && isTimestamp(value);
  if (!(known(left) && known(right))) return known(left) ? -1 : known(right) ? 1 : 0;
  return compareTimestamps(right, left);
}

export function collaboratorSections(collaborators: IssueCollaborators): CollaboratorSections {
  const roleOf = (agent: IssueCollaboratorAgent): LegionRole | undefined =>
    LEGION_ROLES.find((role) => role === agent.legion?.role);
  const legion = collaborators.agents.filter(
    (agent): agent is LegionAgent => agent.legion !== null && roleOf(agent) !== undefined
  );
  return {
    legion: LEGION_ROLES.flatMap((role) => {
      const agents = legion
        .filter((agent) => agent.legion.role === role)
        .sort((left, right) => newestFirst(left.legion.started_at, right.legion.started_at));
      return agents.length === 0 ? [] : [{ agents, role }];
    }),
    // Every session no Legion section holds, so a registry entry naming a role this build does
    // not know is listed rather than dropped from the page.
    others: collaborators.agents
      .filter((agent) => roleOf(agent) === undefined)
      .sort((left, right) => newestFirst(left.last_at, right.last_at)),
    people: collaborators.people,
  };
}

function SectionHeading({ children }: { children: ReactNode }): ReactNode {
  return <h2 className={`text-sm font-semibold ${textSecondaryOnCanvas}`}>{children}</h2>;
}

/** One collaborator's row: who, then the line that says how they worked the issue. `card` carries
 *  the colours and no border width, as every other card in this app pairs it with one.
 *  `data-collaborator` marks the row's own name, which a test selects instead of the row's shape,
 *  since the detail line can name another session (the one that replaced this row's). */
function Row({ detail, name }: { detail: ReactNode; name: ReactNode }): ReactNode {
  return (
    <li className={`min-w-0 rounded-lg border p-3 break-words ${borderDefault} ${card}`}>
      <div className={`min-w-0 font-medium ${textPrimaryOnSurface}`} data-collaborator="">
        {name}
      </div>
      <div
        className={`mt-1 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-sm ${textMutedOnSurface}`}
      >
        {detail}
      </div>
    </li>
  );
}

/** How a collaborator worked the issue - `assignee`, `claimed`, `posted`, `answered` - and when
 *  they last did. */
function Worked({
  lastAt,
  reasons,
}: {
  lastAt: string | null;
  reasons: IssueCollaboratorPerson["reasons"];
}): ReactNode {
  return (
    <>
      {reasons.length === 0 ? null : <span>{reasons.join(", ")}</span>}
      {lastAt === null ? null : <Timestamp at={lastAt} />}
    </>
  );
}

/**
 * Everyone who worked the issue, running or not (`GET /issues/{key}/collaborators`): Legion's
 * agents by role, other agent sessions, and people. Each agent is an `AgentName` link to its live
 * conversation, its running state from the agents registry, so a replaced or stopped agent is one
 * click from the conversation it had; a person is their login as plain text, as everywhere.
 */
export function CollaboratorsTab({ issueKey }: { issueKey: string }): ReactNode {
  const query = useQuery({
    queryFn: () => api.getIssueCollaborators(issueKey),
    queryKey: ["issue", issueKey, "collaborators"],
  });
  if (query.isPending) {
    return <p className={textMutedOnCanvas}>Loading collaborators…</p>;
  }
  if (query.isError) {
    return (
      <QueryError
        message="Couldn't load who worked this issue."
        onRetry={() => void query.refetch()}
        retrying={query.isFetching}
      />
    );
  }
  const collaborators = query.data;
  const sections = collaboratorSections(collaborators);
  const titles = new Map(
    collaborators.agents.map((agent) => [agent.session_id, agent.title ?? undefined])
  );
  const unavailable = collaborators.legion_registry === "unavailable";
  const empty = collaborators.agents.length === 0 && collaborators.people.length === 0;
  return (
    <div className="min-w-0 space-y-6">
      {unavailable ? (
        <p className={`text-sm ${textMutedOnCanvas}`} role="status">
          Legion&apos;s record of this issue&apos;s agents could not be read.
        </p>
      ) : null}
      {empty && !unavailable ? (
        <EmptyState label="Collaborators empty state" message="No one has worked this issue yet." />
      ) : null}
      {sections.legion.length === 0 ? null : (
        <section aria-label="Legion agents" className="space-y-3">
          <SectionHeading>Legion agents</SectionHeading>
          {sections.legion.map(({ agents, role }) => (
            <section aria-label={role} className="space-y-2" key={role}>
              <h3 className={`text-xs font-semibold uppercase ${textMutedOnCanvas}`}>{role}</h3>
              <ul className="space-y-2">
                {agents.map((agent) => (
                  <Row
                    detail={
                      <>
                        <span>
                          started <Timestamp at={agent.legion.started_at} />
                        </span>
                        {agent.legion.replaced_by === null ? null : (
                          <span data-replaced-by={agent.legion.replaced_by}>
                            replaced by{" "}
                            <AgentName
                              inline
                              sessionId={agent.legion.replaced_by}
                              storedTitle={titles.get(agent.legion.replaced_by)}
                            />
                          </span>
                        )}
                      </>
                    }
                    key={agent.session_id}
                    name={
                      <AgentName
                        sessionId={agent.session_id}
                        storedTitle={agent.title ?? undefined}
                      />
                    }
                  />
                ))}
              </ul>
            </section>
          ))}
        </section>
      )}
      {sections.others.length === 0 ? null : (
        <section aria-label="Other agents" className="space-y-2">
          <SectionHeading>Other agents</SectionHeading>
          <ul className="space-y-2">
            {sections.others.map((agent) => (
              <Row
                detail={<Worked lastAt={agent.last_at} reasons={agent.reasons} />}
                key={agent.session_id}
                name={
                  <AgentName sessionId={agent.session_id} storedTitle={agent.title ?? undefined} />
                }
              />
            ))}
          </ul>
        </section>
      )}
      {sections.people.length === 0 ? null : (
        <section aria-label="People" className="space-y-2">
          <SectionHeading>People</SectionHeading>
          <ul className="space-y-2">
            {sections.people.map((person) => (
              <Row
                detail={<Worked lastAt={person.last_at} reasons={person.reasons} />}
                key={person.login}
                name={person.login}
              />
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

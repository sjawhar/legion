import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";

import type { Agent, InboxRow } from "../../api/types";
import { useCappedSearchParam } from "../../lib/query-param";
import type { AgentGroup } from "./agent-groups";

/**
 * What narrows the agent list: any number of groups, machines and roles, and a free-text
 * search. An empty list matches every session, as an empty search box does, and a list of two
 * keeps the rows of either (INFRA-812, R7: each of the three is a multi-select).
 */
export interface AgentFilters {
  readonly groups: readonly AgentGroup[];
  readonly machines: readonly string[];
  readonly roles: readonly string[];
  readonly search: string;
}

/** The free-text query is the page's one URL-backed filter field (`?q=`, through the same
 *  capped, history-`replace`d hook `useIssueFilters` uses); the group, machine and role
 *  selections stay plain component state, as the machine and role did before the box. */
export function useAgentSearch(): readonly [string, (next: string) => void] {
  return useCappedSearchParam("q");
}

/**
 * The machine a link into this page named (`/agents?machine=<host>`), taken once and then
 * removed from the address, so the machine filter has one owner: the control. The machines page
 * of dispatch://INFRA-883 links here that way, and a host with no live session must still read
 * as a machine the person picked, which it does, since the control keeps a picked value as a row
 * it can untick. Leaving the parameter in place would make a reload undo the clearing.
 */
export function useMachineFromAddress(): readonly string[] {
  const [searchParams, setSearchParams] = useSearchParams();
  const [taken] = useState(() => searchParams.get("machine") ?? "");
  useEffect(() => {
    if (taken === "") return;
    setSearchParams(
      (current) => {
        const params = new URLSearchParams(current.toString());
        params.delete("machine");
        return params;
      },
      { replace: true }
    );
  }, [setSearchParams, taken]);
  return taken === "" ? EMPTY_MACHINES : [taken];
}

/** One frozen empty list, so the page's filter state is not new on every render. */
const EMPTY_MACHINES: readonly string[] = [];

/** Every word `search` names, lowercased; an empty query has none. The page splits this once per
 *  render (`AgentsPage`'s `words`) rather than once per agent, since every row's `matchesFilters`
 *  call would otherwise re-split the one query it shares. */
export function searchWords(search: string): readonly string[] {
  return search.trim().toLowerCase().split(/\s+/).filter(Boolean);
}

/** The text a session's search words match against: its title, directory, machine, session id,
 *  and the key of every issue one of its OPEN asks names - once, lowercased, so a session with
 *  no open-ask issue still matches on the other four. An issue the session answered or that
 *  closed since drops out of this list with the ask (the inbox read this comes from is open asks
 *  only), so a search by that issue's key stops finding the session. */
function searchHaystack(agent: Agent, issueKeys: readonly string[]): string {
  return [agent.title, agent.dir, agent.host, agent.session_id, ...issueKeys]
    .join(" ")
    .toLowerCase();
}

/** `words` is `searchWords(filters.search)`, split once by the caller rather than once per agent
 *  (the page's `matches` closure runs this for every row). `group` is the row's own state
 *  (`agent-groups.ts`), which the page has already worked out to place the row. `issueKeys` is
 *  every issue key an open ask by this session names (`sessionIssueKeys`, below), the free-text
 *  search's only field beyond what `Agent` itself carries. */
export function matchesFilters(
  agent: Agent,
  group: AgentGroup,
  filters: Pick<AgentFilters, "groups" | "machines" | "roles">,
  words: readonly string[],
  issueKeys: readonly string[]
): boolean {
  if (filters.groups.length > 0 && !filters.groups.includes(group)) return false;
  if (filters.machines.length > 0 && !filters.machines.includes(agent.host)) return false;
  if (filters.roles.length > 0 && !filters.roles.some((role) => agent.roles.includes(role))) {
    return false;
  }
  if (words.length === 0) return true;
  const haystack = searchHaystack(agent, issueKeys);
  return words.every((word) => haystack.includes(word));
}

/** The machines and roles the live sessions actually occupy: a filter can only offer what is
 *  there, so a stale option can never hide every agent. */
export function filterOptions(agents: readonly Agent[]): {
  machines: string[];
  roles: string[];
} {
  const machines = new Set<string>();
  const roles = new Set<string>();
  for (const agent of agents) {
    if (agent.host !== "") machines.add(agent.host);
    for (const role of agent.roles) roles.add(role);
  }
  return { machines: [...machines].sort(), roles: [...roles].sort() };
}

/** Every issue key an OPEN ask by each session names, keyed by session id - the search box's
 *  "issue key" field. `rows` is the inbox read the page already fetches for its `needsYou`
 *  counts (`GET /api/v1/inbox`, open asks only: `inbox.go`'s `where a.state = 'open'`), so this
 *  costs no network call of its own, and a session's answered or resolved asks name no issue
 *  here. */
export function sessionIssueKeys(
  rows: readonly InboxRow[]
): ReadonlyMap<string, readonly string[]> {
  const keys = new Map<string, Set<string>>();
  for (const ask of rows) {
    if (ask.author.kind !== "session" || ask.issue_key === null) continue;
    const forSession = keys.get(ask.author.id) ?? new Set<string>();
    forSession.add(ask.issue_key);
    keys.set(ask.author.id, forSession);
  }
  return new Map([...keys].map(([session, forSession]) => [session, [...forSession]]));
}

import type { Agent, InboxRow } from "../../api/types";
import { useCappedSearchParam } from "../../lib/query-param";

/**
 * What narrows the agent list, mirroring the `envoy broadcast` script's own selectors: one
 * machine, one role, and a free-text search. An empty field matches everything.
 */
export interface AgentFilters {
  readonly machine: string;
  readonly role: string;
  readonly search: string;
}

/** The free-text query is the page's one URL-backed filter field (`?q=`, through the same
 *  capped, history-`replace`d hook `useIssueFilters` uses); `machine` and `role` stay plain
 *  component state, as they were before this box. */
export function useAgentSearch(): readonly [string, (next: string) => void] {
  return useCappedSearchParam("q");
}

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
  return [agent.title, agent.dir, agent.machine_id, agent.session_id, ...issueKeys]
    .join(" ")
    .toLowerCase();
}

/** `words` is `searchWords(filters.search)`, split once by the caller rather than once per agent
 *  (the page's `matches` closure runs this for every row). `issueKeys` is every issue key an open
 *  ask by this session names (`sessionIssueKeys`, below), the free-text search's only field
 *  beyond what `Agent` itself carries. */
export function matchesFilters(
  agent: Agent,
  filters: Pick<AgentFilters, "machine" | "role">,
  words: readonly string[],
  issueKeys: readonly string[]
): boolean {
  if (filters.machine !== "" && agent.machine_id !== filters.machine) return false;
  if (filters.role !== "" && !agent.roles.includes(filters.role)) return false;
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
    if (agent.machine_id !== "") machines.add(agent.machine_id);
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

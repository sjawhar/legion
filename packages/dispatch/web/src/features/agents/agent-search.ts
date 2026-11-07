import type { Agent, InboxRow } from "../../api/types";
import { useCappedSearchParam } from "../../lib/query-param";
import { AGENTS_MACHINE_PARAM } from "../refs/routes";

/**
 * What narrows the agent list, mirroring the `envoy broadcast` script's own selectors: one
 * machine, one role, and a free-text search. An empty field matches everything.
 */
export interface AgentFilters {
  readonly machine: string;
  readonly role: string;
  readonly search: string;
}

/** The page's two URL-backed filter fields, each through the same capped, history-`replace`d
 *  hook `useIssueFilters` uses: the free-text query (`?q=`) and the machine (`?machine=`).
 *  `role` stays plain component state, as it was before this box. Both parameters live here
 *  rather than in the page, so the control around them can be rebuilt without moving them. */
export function useAgentSearch(): readonly [string, (next: string) => void] {
  return useCappedSearchParam("q");
}

/** The machine filter, which is the address's own `?machine=` rather than part of `?q=`: the
 *  free-text query matches the directory as well as the machine, so `?q=<host>` would also find
 *  a session whose directory holds the machine's name, while `/machines/<machine>`'s link to
 *  `/agents?machine=<host>` must name exactly the sessions on that machine. */
export function useAgentMachine(): readonly [string, (next: string) => void] {
  return useCappedSearchParam(AGENTS_MACHINE_PARAM);
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
  return [agent.title, agent.dir, agent.host, agent.session_id, ...issueKeys]
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
  if (filters.machine !== "" && agent.host !== filters.machine) return false;
  if (filters.role !== "" && !agent.roles.includes(filters.role)) return false;
  if (words.length === 0) return true;
  const haystack = searchHaystack(agent, issueKeys);
  return words.every((word) => haystack.includes(word));
}

/**
 * The machines and roles the filter control offers: the ones the live sessions occupy, and the
 * machine the filter already names wherever no live session reports it.
 *
 * That last one is the whole of this rule. `?machine=` can name a host nothing live reports —
 * an old link, a machine whose sessions have ended, or `/machines/<machine>`'s own "Agents on
 * this machine" link, which that page shows even when it draws nothing. The list is then
 * honestly empty, which is the filter doing its job; but with the live hosts alone for options
 * the control would show no value, read `All machines` while every row was hidden, and fire no
 * change when the reader picked `All machines` — the one control that seems to show the filter
 * could not clear it. Offering the named machine keeps the control showing the filter in force,
 * so a stale option can never hide every agent with no way back.
 */
export function filterOptions(
  agents: readonly Agent[],
  machine: string
): {
  machines: string[];
  roles: string[];
} {
  const machines = new Set<string>();
  const roles = new Set<string>();
  if (machine !== "") machines.add(machine);
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

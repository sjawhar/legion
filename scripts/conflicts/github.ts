import { run, success } from "./git.ts";
import type { GraphCommit, GraphForcePush, GraphPullRequest } from "./types.ts";

/** The most pull requests one `gh pr list` call reads (GitHub's search returns at most this many
 * results); a list this long may be cut short. */
const LIST_CAP = 1000;

/** A pull request as `gh pr list` gives it. */
type Listed = { number: number; headRefName: string };

/** The pull request's fields, a page of its commits and its force pushes, as the Python run read
 * them, plus `closedAt`. */
const QUERY = `
query($owner:String!, $name:String!, $n:Int!, $after:String) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$n) {
      number title state baseRefName headRefName headRefOid mergedAt createdAt closedAt
      commits(first:100, after:$after) {
        pageInfo { hasNextPage endCursor }
        nodes { commit { oid messageHeadline committedDate parents(first:5) { nodes { oid } } } }
      }
      timelineItems(itemTypes:[HEAD_REF_FORCE_PUSHED_EVENT], first:100) {
        nodes { ... on HeadRefForcePushedEvent { createdAt beforeCommit { oid } afterCommit { oid } } }
      }
    }
  }
}
`;

interface QueryPage {
  data: {
    repository: {
      pullRequest:
        | (Omit<GraphPullRequest, "commits" | "forcePushes"> & {
            commits: {
              pageInfo: { hasNextPage: boolean; endCursor: string | null };
              nodes: Array<{ commit: GraphCommit }>;
            };
            timelineItems: { nodes: Array<Partial<GraphForcePush> | null> };
          })
        | null;
    };
  };
}

/** Runs `gh` and parses its JSON output; throws naming the command and its stderr on failure. */
async function gh<T>(args: string[]): Promise<T> {
  return JSON.parse((await run(["gh", ...args], success)).stdout) as T;
}

/** `gh pr list --repo <repo>` with `args`; throws when the list reaches the cap, giving `count`
 * (what the pull requests listed are) and `advice`. */
async function listOnce(
  repo: string,
  args: string[],
  count: string,
  advice: string
): Promise<Listed[]> {
  const prs = await gh<Listed[]>([
    "pr",
    "list",
    "--repo",
    repo,
    ...args,
    "--limit",
    `${LIST_CAP}`,
    "--json",
    "number,headRefName",
  ]);
  if (prs.length >= LIST_CAP) {
    throw new Error(
      `${prs.length} pull requests ${count}, the list's cap of ${LIST_CAP}: ${advice}`
    );
  }
  return prs;
}

/** Each pull request of `lists` once, by number, sorted by number. */
export function unionByNumber<T extends { number: number }>(...lists: T[][]): T[] {
  const byNumber = new Map<number, T>();
  for (const list of lists) {
    for (const pr of list) if (!byNumber.has(pr.number)) byNumber.set(pr.number, pr);
  }
  return [...byNumber.values()].sort((a, b) => a.number - b.number);
}

/** Every pull request of `repo` (`owner/name` on GitHub), any state, updated at or after
 * `sinceInstant`, and every one open now, each once and sorted by number; read with the `gh`
 * CLI's credentials. */
export async function listPullRequests(repo: string, sinceInstant: string): Promise<Listed[]> {
  const [updated, open] = await Promise.all([
    listOnce(
      repo,
      ["--state", "all", "--search", `updated:>=${sinceInstant}`],
      `were updated since ${sinceInstant}`,
      "the window is too large for one search; measure a shorter window"
    ),
    listOnce(repo, ["--state", "open"], "are open", "too many for one list"),
  ]);
  return unionByNumber(updated, open);
}

/** Pull request `number` of `repo` (`owner/name` on GitHub), every page of its commits read. */
export async function fetchPullRequest(repo: string, number: number): Promise<GraphPullRequest> {
  const [owner, name] = repo.split("/");
  const commits: GraphCommit[] = [];
  let after: string | null = null;
  for (;;) {
    const page: QueryPage = await gh<QueryPage>([
      "api",
      "graphql",
      "-f",
      `query=${QUERY}`,
      "-f",
      `owner=${owner}`,
      "-f",
      `name=${name}`,
      "-F",
      `n=${number}`,
      ...(after ? ["-f", `after=${after}`] : []),
    ]);
    const pr = page.data.repository.pullRequest;
    if (pr === null) throw new Error(`${repo}#${number} is not a pull request`);
    commits.push(...pr.commits.nodes.map((node) => node.commit));
    if (!pr.commits.pageInfo.hasNextPage) {
      const { commits: _, timelineItems, ...fields } = pr;
      return {
        ...fields,
        commits,
        forcePushes: timelineItems.nodes.filter(
          (node): node is GraphForcePush => node?.createdAt !== undefined
        ),
      };
    }
    after = pr.commits.pageInfo.endCursor;
  }
}

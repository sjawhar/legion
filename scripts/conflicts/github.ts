import { run, success } from "./git.ts";
import type { GraphCommit, GraphForcePush, GraphPullRequest } from "./types.ts";

/** GitHub's search returns at most this many results; a list this long may be cut short. */
const SEARCH_CAP = 1000;

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

/** Every pull request of `repo` (`owner/name` on GitHub), any state, updated at or after
 * `sinceInstant`, read with the `gh` CLI's credentials. */
export async function listPullRequests(
  repo: string,
  sinceInstant: string
): Promise<Array<{ number: number; headRefName: string }>> {
  const prs = await gh<Array<{ number: number; headRefName: string }>>([
    "pr",
    "list",
    "--repo",
    repo,
    "--state",
    "all",
    "--limit",
    `${SEARCH_CAP}`,
    "--search",
    `updated:>=${sinceInstant}`,
    "--json",
    "number,headRefName",
  ]);
  if (prs.length >= SEARCH_CAP) {
    throw new Error(
      `${prs.length} pull requests were updated since ${sinceInstant}, GitHub's search cap: ` +
        "the window is too large for one search; measure a shorter window"
    );
  }
  return prs;
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

import { run, success } from "./git.ts";
import type { GraphCommit, GraphForcePush, GraphPullRequest } from "./types.ts";

/** GitHub's search returns at most this many results; a list this long may be cut short. */
const SEARCH_CAP = 1000;

/** The pull request's fields, a page of its commits and its force pushes, as the Python run read
 * them. */
const QUERY = `
query($owner:String!, $name:String!, $n:Int!, $after:String) {
  repository(owner:$owner, name:$name) {
    pullRequest(number:$n) {
      number title state baseRefName headRefName headRefOid mergedAt createdAt
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

/** Where the pull requests come from: GitHub through `gh`. */
interface PullRequestSource {
  /** Every pull request, any state, updated at or after `sinceInstant`. */
  list(sinceInstant: string): Promise<Array<{ number: number; headRefName: string }>>;
  fetch(number: number): Promise<GraphPullRequest>;
}

/** Runs `gh` and parses its JSON output; throws naming the command and its stderr on failure. */
async function gh<T>(args: string[]): Promise<T> {
  return JSON.parse((await run(["gh", ...args], success)).stdout) as T;
}

/** `repo`'s pull requests as `owner/name` on GitHub, read with the `gh` CLI's credentials. */
export function ghSource(repo: string): PullRequestSource {
  const [owner, name] = repo.split("/");
  return {
    async list(sinceInstant) {
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
    },

    async fetch(number) {
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
    },
  };
}

import { CliError } from "./errors";

export type Fetch = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

export const GITHUB_GRAPHQL_URL = "https://api.github.com/graphql";

/** One GraphQL call against GitHub as the App whose token was redeemed. Rejects with a CliError
 * carrying GitHub's own message (HTTP status + body for a transport failure, the `errors[]`
 * messages for a GraphQL-level refusal such as `Resource not accessible by integration`). */
export type GraphqlCall = <T>(query: string, variables: Record<string, unknown>) => Promise<T>;

export interface GitHubRepo {
  owner: string;
  name: string;
}

/** An unresolved review thread reduced to what the acceptance rule reads. A thread has no URL of
 * its own on GitHub; `url` is its opening comment's, the anchor the PR page scrolls to. */
export interface UnresolvedThread {
  id: string;
  url: string;
  openerLogin: string | null;
  newestLogin: string | null;
  newestBody: string;
}

interface Actor {
  login: string;
}

interface ThreadsPage {
  repository: {
    pullRequest: {
      reviewThreads: {
        pageInfo: { hasNextPage: boolean; endCursor: string | null };
        nodes: Array<{
          id: string;
          isResolved: boolean;
          opener: { nodes: Array<{ url: string; author: Actor | null }> };
          newest: { nodes: Array<{ author: Actor | null; body: string }> };
        }>;
      };
    } | null;
  } | null;
}

const THREADS_QUERY = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          opener: comments(first: 1) { nodes { url author { login } } }
          newest: comments(last: 1) { nodes { author { login } body } }
        }
      }
    }
  }
}`;

const RESOLVE_MUTATION = `mutation($threadId: ID!) {
  resolveReviewThread(input: { threadId: $threadId }) { thread { id isResolved } }
}`;

export function githubGraphql(fetch: Fetch, token: string): GraphqlCall {
  return async <T>(query: string, variables: Record<string, unknown>): Promise<T> => {
    const response = await fetch(GITHUB_GRAPHQL_URL, {
      method: "POST",
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
      body: JSON.stringify({ query, variables }),
    });
    if (!response.ok) {
      throw new CliError(
        `GitHub GraphQL request failed (${response.status}): ${await response.text()}`
      );
    }
    const payload = (await response.json()) as { data?: T; errors?: Array<{ message: string }> };
    if (payload.errors && payload.errors.length > 0) {
      throw new CliError(payload.errors.map((error) => error.message).join("; "));
    }
    if (payload.data === undefined) throw new CliError("GitHub GraphQL response carried no data");
    return payload.data;
  };
}

/** The reviewer's acceptance reply form: the comment's first non-blank line begins `Accepted:`
 * (`Accepted: fixed in <commit> — <one line>` or `Accepted: not a defect — <reason>`). Anything
 * else — `Still open: …`, `Accepted (round 2): …`, a bare "fixed" — is not an acceptance. */
export function isAcceptance(body: string): boolean {
  return body.trimStart().startsWith("Accepted:");
}

/** True when the thread's newest comment is its opener's own `Accepted:` reply: the account that
 * raised the point is the one closing it, and nobody has replied since. */
export function acceptedByOpener(thread: UnresolvedThread): boolean {
  return (
    thread.openerLogin !== null &&
    thread.openerLogin === thread.newestLogin &&
    isAcceptance(thread.newestBody)
  );
}

/** Every unresolved review thread on the pull request, across every page of `reviewThreads`. */
export async function listUnresolvedThreads(
  graphql: GraphqlCall,
  repo: GitHubRepo,
  number: number
): Promise<UnresolvedThread[]> {
  const threads: UnresolvedThread[] = [];
  let after: string | null = null;
  do {
    const page: ThreadsPage = await graphql<ThreadsPage>(THREADS_QUERY, {
      owner: repo.owner,
      name: repo.name,
      number,
      after,
    });
    const pullRequest = page.repository?.pullRequest;
    if (!pullRequest) {
      throw new CliError(`${repo.owner}/${repo.name}#${number} was not found by GitHub`);
    }
    for (const node of pullRequest.reviewThreads.nodes) {
      if (node.isResolved) continue;
      const opener = node.opener.nodes[0];
      const newest = node.newest.nodes[0];
      if (!opener || !newest) throw new CliError(`review thread ${node.id} has no comments`);
      threads.push({
        id: node.id,
        url: opener.url,
        openerLogin: opener.author?.login ?? null,
        newestLogin: newest.author?.login ?? null,
        newestBody: newest.body,
      });
    }
    const { pageInfo } = pullRequest.reviewThreads;
    after = pageInfo.hasNextPage ? pageInfo.endCursor : null;
  } while (after !== null);
  return threads;
}

/** One `resolveReviewThread` mutation; a refusal surfaces as the CliError `githubGraphql` throws. */
export async function resolveThread(graphql: GraphqlCall, threadId: string): Promise<void> {
  await graphql(RESOLVE_MUTATION, { threadId });
}

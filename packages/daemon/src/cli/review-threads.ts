import { CliError } from "./errors";

export type Fetch = (input: string | URL | Request, init?: RequestInit) => Promise<Response>;

const GITHUB_GRAPHQL_URL = "https://api.github.com/graphql";

/** One GraphQL call against GitHub as the identity its transport carries: the App whose token a
 * grant redeemed (`githubGraphql`), or whoever the caller's own `gh` authenticates as
 * (`ghGraphql`). Rejects with a CliError carrying GitHub's own message (HTTP status + body for a
 * transport failure, gh's stderr for a failed `gh`, the `errors[]` messages for a GraphQL-level
 * refusal such as `Resource not accessible by integration`). */
export type GraphqlCall = <T>(query: string, variables: Record<string, unknown>) => Promise<T>;

export interface GitHubRepo {
  owner: string;
  name: string;
}

/** An unresolved review thread reduced to what the acceptance rule reads. A thread has no URL of
 * its own on GitHub; `url` is its opening comment's, the anchor the PR page scrolls to.
 * `newestPending` marks a newest comment that is a draft in a pending, unsubmitted review. */
interface UnresolvedThread {
  id: string;
  url: string;
  openerLogin: string | null;
  newestLogin: string | null;
  newestBody: string;
  newestPending: boolean;
  /** A thread a bot account opened that is not a Legion role's: neither its opening comment nor
   * its review carries the Legion footer. A Legion reviewer's thread closes only on its own
   * `Accepted:`; a bot never posts one. */
  botOpened: boolean;
  /** The pull request's author, who answers a bot's thread with a disposition. */
  author: string | null;
}

interface Actor {
  login: string;
  __typename?: string;
}

interface ThreadsPage {
  repository: {
    pullRequest: {
      author: Actor | null;
      reviewThreads: {
        pageInfo: { hasNextPage: boolean; endCursor: string | null };
        nodes: Array<{
          id: string;
          isResolved: boolean;
          opener: {
            nodes: Array<{
              url: string;
              body: string;
              author: Actor | null;
              pullRequestReview: { body: string } | null;
            }>;
          };
          newest: {
            // `state` is absent only when the query stops selecting it; listUnresolvedThreads refuses that.
            nodes: Array<{ author: Actor | null; body: string; state?: "PENDING" | "SUBMITTED" }>;
          };
        }>;
      };
    } | null;
  } | null;
}

const THREADS_QUERY = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      author { login }
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          opener: comments(first: 1) { nodes { url body author { __typename login } pullRequestReview { body } } }
          newest: comments(last: 1) { nodes { author { login } body state } }
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
    return graphqlData<T>(await response.json());
  };
}

/** Runs `gh` with `args`, `stdin` on its standard input, and `env` as its whole environment. */
export type RunGh = (
  args: string[],
  stdin: string,
  env: NodeJS.ProcessEnv
) => Promise<{ exitCode: number; stdout: string; stderr: string }>;

/** One GraphQL call through the caller's own `gh` (`gh api graphql --input -`), for a session
 * outside a Legion pane, which has no grant to redeem. gh gets the caller's environment, which
 * decides whose credential it uses, with GH_REPO set to the repository, so a `gh` that picks its
 * credential by repository (the devbox shim routes to that owner's GitHub App) authenticates for
 * it from any directory. A non-zero exit rejects with a CliError carrying gh's own message; a
 * successful call hands gh's stderr to `stderr` verbatim, since that is where such a `gh` says the
 * call acts as someone else (an inherited GH_TOKEN, a fallback personal token). */
export function ghGraphql(
  runGh: RunGh,
  env: NodeJS.ProcessEnv,
  repo: GitHubRepo,
  stderr: (text: string) => void
): GraphqlCall {
  return async <T>(query: string, variables: Record<string, unknown>): Promise<T> => {
    const result = await runGh(
      ["api", "graphql", "--input", "-"],
      JSON.stringify({ query, variables }),
      { ...env, GH_REPO: `${repo.owner}/${repo.name}` }
    );
    if (result.exitCode !== 0) {
      const message = result.stderr.trim() || result.stdout.trim();
      throw new CliError(`gh api graphql failed (exit ${result.exitCode}): ${message}`);
    }
    if (result.stderr !== "") stderr(result.stderr);
    return graphqlData<T>(JSON.parse(result.stdout));
  };
}

/** A GraphQL response's `data`, or a CliError with its `errors[]` messages (a GraphQL-level
 * refusal such as `Resource not accessible by integration`). */
function graphqlData<T>(response: unknown): T {
  const payload = response as { data?: T; errors?: Array<{ message: string }> };
  if (payload.errors && payload.errors.length > 0) {
    throw new CliError(payload.errors.map((error) => error.message).join("; "));
  }
  if (payload.data == null) throw new CliError("GitHub GraphQL response carried no data");
  return payload.data;
}

/** The reviewer's acceptance reply form: the comment begins `Accepted:` after any leading space,
 * tab, CR or LF (`Accepted: fixed in <commit> — <one line>` or `Accepted: not a defect —
 * <reason>`). Anything else — `Still open: …`, `Accepted (round 2): …`, a bare "fixed", or
 * `Accepted:` after other whitespace such as a no-break space — is not an acceptance. The Go CLI
 * trims the same four characters (`threads.go`), so both CLIs apply one rule. */
function isAcceptance(body: string): boolean {
  return body.replace(/^[ \t\r\n]+/, "").startsWith("Accepted:");
}

/** True when the thread's newest comment is its opener's own submitted `Accepted:` reply: the
 * account that raised the point is the one closing it, and nobody has replied since. A draft in a
 * pending review never counts. GitHub shows a draft only to its author, so without this check an
 * outside-a-pane caller posting as the opener's account would resolve on an acceptance the
 * reviewer has not submitted. For every caller, then, a thread is resolved only when its newest
 * submitted comment is the opener's `Accepted:`. A caller still sees its own drafts, and one newer
 * than a submitted acceptance can only make that caller leave the thread open. */
function acceptedByOpener(thread: UnresolvedThread): boolean {
  return (
    !thread.newestPending &&
    thread.openerLogin !== null &&
    thread.openerLogin === thread.newestLogin &&
    isAcceptance(thread.newestBody)
  );
}

/** What a Legion role posts on GitHub carries this marker (the listener reads the same one). */
const LEGION_FOOTER = "<!-- legion:";

/** The pull request author's answer to a bot's thread, as its reply's first line (after the same
 * leading space, tab, CR or LF `Accepted:` may follow): `Fixed in <commit>: <what changed>` or
 * `Declined: <reason>`, the implementer's reply grammar for every review thread. Anything below
 * the first line is the reader's; the gate reads only line one. */
function isDisposition(body: string): boolean {
  const first = (body.replace(/^[ \t\r\n]+/, "").split("\n")[0] ?? "").replace(/\r$/, "");
  return /^(?:Fixed in [0-9a-f]{7,40}|Declined): \S/.test(first);
}

/** True when a bot's thread was answered, in its newest submitted comment, by the pull request's
 * author with a disposition. A bot never posts `Accepted:`, so without this a thread a CI bot
 * opens could never close. The Go CLI applies the same rule (`threads.go` disposedByAuthor). */
function disposedByAuthor(thread: UnresolvedThread): boolean {
  return (
    !thread.newestPending &&
    thread.botOpened &&
    thread.author !== null &&
    thread.newestLogin === thread.author &&
    thread.newestLogin !== thread.openerLogin &&
    isDisposition(thread.newestBody)
  );
}

/** Every unresolved review thread on the pull request, across every page of `reviewThreads`. */
async function listUnresolvedThreads(
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
      // GitHub always answers `state` when the query selects it. Anything else means the query or
      // the response changed shape, and reading it as submitted would resolve on a draft.
      if (newest.state !== "PENDING" && newest.state !== "SUBMITTED") {
        throw new CliError(
          `review thread ${node.id}: its newest comment carried state ${JSON.stringify(newest.state)}, not "PENDING" or "SUBMITTED"`
        );
      }
      threads.push({
        id: node.id,
        url: opener.url,
        openerLogin: opener.author?.login ?? null,
        newestLogin: newest.author?.login ?? null,
        newestBody: newest.body,
        newestPending: newest.state === "PENDING",
        botOpened:
          opener.author?.__typename === "Bot" &&
          !opener.body.includes(LEGION_FOOTER) &&
          !(opener.pullRequestReview?.body ?? "").includes(LEGION_FOOTER),
        author: pullRequest.author?.login ?? null,
      });
    }
    const { pageInfo } = pullRequest.reviewThreads;
    after = pageInfo.hasNextPage ? pageInfo.endCursor : null;
  } while (after !== null);
  return threads;
}

/** One `resolveReviewThread` mutation; a refusal surfaces as the CliError `graphql` throws. */
async function resolveThread(graphql: GraphqlCall, threadId: string): Promise<void> {
  await graphql(RESOLVE_MUTATION, { threadId });
}

/** `--repo <owner>/<name>`, or the CliError the operator sees. */
export function parseRepo(value: string): GitHubRepo {
  const match = /^([^/\s]+)\/([^/\s]+)$/.exec(value);
  if (!match) throw new CliError(`--repo must be <owner>/<name> (got ${JSON.stringify(value)})`);
  return { owner: match[1] as string, name: match[2] as string };
}

/** `--pr <number>`, or the CliError the operator sees. */
export function parsePullNumber(value: string): number {
  if (!/^\d+$/.test(value)) {
    throw new CliError(`--pr must be a pull request number (got ${JSON.stringify(value)})`);
  }
  return Number(value);
}

/** The policy `legion threads resolve` applies, as whichever identity `graphql` carries: every
 * unresolved review thread whose newest comment is its opener's own submitted `Accepted:` reply,
 * or, on a thread a bot opened, the pull request author's submitted disposition, is resolved (one `resolveReviewThread` per thread, in GitHub's order) and every other unresolved
 * thread is named as left open; no unresolved thread at all prints exactly `no unresolved
 * threads`. A thread GitHub refuses rejects with a CliError naming the thread's URL and GitHub's
 * message, and nothing after it is attempted. */
export async function resolveAcceptedThreads(
  graphql: GraphqlCall,
  repo: GitHubRepo,
  number: number,
  log: (line: string) => void
): Promise<void> {
  const threads = await listUnresolvedThreads(graphql, repo, number);
  if (threads.length === 0) {
    log("no unresolved threads");
    return;
  }
  for (const thread of threads) {
    if (!acceptedByOpener(thread) && !disposedByAuthor(thread)) {
      const by = thread.newestLogin ?? "an unknown account";
      const reason = thread.newestPending
        ? "an unsubmitted draft in a pending review"
        : thread.botOpened
          ? "not the opener's acceptance or the pull request author's disposition (Fixed in <commit>: … or Declined: …)"
          : "not an acceptance";
      log(`left open ${thread.url} — newest reply by ${by} is ${reason}`);
      continue;
    }
    try {
      await resolveThread(graphql, thread.id);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      throw new CliError(`resolveReviewThread failed for ${thread.url}: ${message}`);
    }
    log(`resolved ${thread.url}`);
  }
}

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

/** An unresolved review thread reduced to the facts the rule reads. A thread has no URL of its own
 * on GitHub; `url` is its opening comment's, the anchor the PR page scrolls to. `newestPending`
 * marks a newest comment that is a draft in a pending, unsubmitted review. */
interface UnresolvedThread {
  id: string;
  url: string;
  openerLogin: string | null;
  openerTypename: string | null;
  newestLogin: string | null;
  newestBody: string;
  newestPending: boolean;
}

/** Each Legion role App's login, keyed by its App role, as the daemon's gh-token answer names them
 * (`<slug>[bot]`); absent when it could not read every one. */
export interface LegionAppLogins {
  implement: string;
  review: string;
}

/** Legion's role Apps as the rule reads them: every App's login through `botSlug`, and the review
 * App's. Null is a session that cannot know them (`--gh`, or a daemon that named none), and then
 * no thread counts as a bot's. */
type LegionApps = { logins: ReadonlySet<string>; review: string } | null;

interface Actor {
  login: string;
  __typename?: string;
}

interface ThreadsPage {
  repository: {
    pullRequest: {
      reviewThreads: {
        pageInfo: { hasNextPage: boolean; endCursor: string | null };
        nodes: Array<{
          id: string;
          isResolved: boolean;
          opener: {
            nodes: Array<{
              url: string;
              author: Actor | null;
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
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          opener: comments(first: 1) { nodes { url author { __typename login } } }
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

/** How both ends of every login comparison read an account: GitHub GraphQL names a Bot by its bare
 * slug and the daemon by its git identity, `<slug>[bot]`, and a login's case never distinguishes
 * two accounts. */
function botSlug(login: string): string {
  return login.replace(/\[bot\]$/, "").toLowerCase();
}

function legionApps(logins: LegionAppLogins | null): LegionApps {
  if (!logins?.review) return null;
  return { logins: new Set(Object.values(logins).map(botSlug)), review: botSlug(logins.review) };
}

/** A reply's first line, after any leading space, tab, CR or LF. */
function firstLine(body: string): string {
  return (body.replace(/^[ \t\r\n]+/, "").split("\n")[0] ?? "").replace(/\r$/, "");
}

/** The reviewer's acceptance reply form: its first line begins `Accepted:` (`Accepted: fixed in
 * <commit> — <one line>` or `Accepted: not a defect — <reason>`). Only space, tab, CR and LF may
 * precede it; `Still open: …`, `Accepted (round 2): …`, a bare "fixed", or `Accepted:` after other
 * whitespace such as a no-break space is not an acceptance. The Go CLI trims the same four
 * characters (`threads.go`). */
function isAcceptance(body: string): boolean {
  return firstLine(body).startsWith("Accepted:");
}

type Resolution = { how: string } | { reason: string };

/** Whether a thread is resolved and on whose acceptance, or, when it is left open, why. The subject
 * of a finding never closes it: a thread closes only on its newest submitted comment being an
 * `Accepted:` from its opener, or, on a thread a Bot that is none of Legion's role Apps opened,
 * from Legion's review App. GitHub cannot tell a CI bot from a person whose `gh` is routed to an
 * App, and such a bot may never accept, so the Legion reviewer is the independent party who
 * adjudicates its finding; the reviewer may accept a finding an App-routed person raised, which
 * the resolved line then says. The pull request's author (the implementer, whose App the merger
 * shares) closes nothing: its reply is an answer, not an acceptance. The reviewer's acceptance need
 * not follow an answer from the author: accepting is the reviewer's judgement of the finding, and a
 * required prior reply would be a ceremony the implementer could satisfy with an empty one. A draft
 * in a pending review never counts, since GitHub shows it only to its author. The Go CLI applies the same rule
 * (`threads.go` resolution), and the two share their vectors. */
function resolution(thread: UnresolvedThread, apps: LegionApps): Resolution {
  if (thread.newestPending) return { reason: "an unsubmitted draft in a pending review" };
  const acceptance = isAcceptance(thread.newestBody);
  if (
    acceptance &&
    thread.openerLogin !== null &&
    thread.newestLogin !== null &&
    botSlug(thread.openerLogin) === botSlug(thread.newestLogin)
  ) {
    return { how: "its opener's acceptance" };
  }
  if (thread.openerTypename !== "Bot") return { reason: "not an acceptance" };
  if (apps === null) {
    return {
      reason:
        "not its opener's acceptance, and this session cannot identify Legion's review App, so a bot's thread closes only on its opener's Accepted:",
    };
  }
  if (thread.openerLogin !== null && apps.logins.has(botSlug(thread.openerLogin))) {
    return { reason: "not an acceptance" };
  }
  if (acceptance && thread.newestLogin !== null && botSlug(thread.newestLogin) === apps.review) {
    return { how: "the Legion reviewer's acceptance of a bot's thread" };
  }
  return { reason: "not its opener's or the Legion reviewer's acceptance" };
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
        openerTypename: opener.author?.__typename ?? null,
        newestLogin: newest.author?.login ?? null,
        newestBody: newest.body,
        newestPending: newest.state === "PENDING",
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
 * unresolved review thread `resolution` finds accepted is resolved (one `resolveReviewThread` per
 * thread, in GitHub's order) and printed as `resolved <url> — <whose acceptance>`, and every other
 * unresolved thread is named as left open with the reason; no unresolved thread at all prints
 * exactly `no unresolved threads`. `legionAppLogins` is the daemon's gh-token answer, null when it
 * named none. A thread GitHub refuses rejects with a CliError naming the thread's URL and
 * GitHub's message, and nothing after it is attempted. */
export async function resolveAcceptedThreads(
  graphql: GraphqlCall,
  repo: GitHubRepo,
  number: number,
  legionAppLogins: LegionAppLogins | null,
  log: (line: string) => void
): Promise<void> {
  const apps = legionApps(legionAppLogins);
  const threads = await listUnresolvedThreads(graphql, repo, number);
  if (threads.length === 0) {
    log("no unresolved threads");
    return;
  }
  for (const thread of threads) {
    const outcome = resolution(thread, apps);
    if ("reason" in outcome) {
      log(
        `left open ${thread.url} — newest reply by ${thread.newestLogin ?? "an unknown account"} is ${outcome.reason}`
      );
      continue;
    }
    try {
      await resolveThread(graphql, thread.id);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      throw new CliError(`resolveReviewThread failed for ${thread.url}: ${message}`);
    }
    log(`resolved ${thread.url} — ${outcome.how}`);
  }
}

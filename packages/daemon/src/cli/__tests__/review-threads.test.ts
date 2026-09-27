import { describe, expect, it } from "bun:test";
import { cmdThreadsResolve } from "../index";

const PR = "https://github.com/sjawhar/legion/pull/993#discussion_r";

interface Comment {
  login: string;
  body: string;
  /** GitHub's `PullRequestReviewCommentState`; a PENDING comment is visible only to its author. */
  state?: "PENDING" | "SUBMITTED";
}

/** Whether a thread's opener is a person or a bot account (GitHub GraphQL's actor `__typename`). */
type OpenerType = "User" | "Bot";

/** One review thread as GitHub's GraphQL serves it; `by` is its opener's type (a Bot's login is its
 * bare slug). */
function thread(
  id: string,
  n: number,
  opener: string,
  newest: Comment,
  { isResolved = false, by = "User" }: { isResolved?: boolean; by?: OpenerType } = {}
) {
  return {
    id,
    isResolved,
    opener: {
      nodes: [{ url: `${PR}${n}`, author: { __typename: by, login: opener } }],
    },
    newest: {
      nodes: [
        { author: { login: newest.login }, body: newest.body, state: newest.state ?? "SUBMITTED" },
      ],
    },
  };
}

function page(nodes: unknown[], endCursor: string | null) {
  return {
    data: {
      repository: {
        pullRequest: {
          reviewThreads: { pageInfo: { hasNextPage: endCursor !== null, endCursor }, nodes },
        },
      },
    },
  };
}

const resolvedOk = (threadId: string) => ({
  data: { resolveReviewThread: { thread: { id: threadId, isResolved: true } } },
});

/** A page as GitHub serves it for `query`: GitHub answers only the fields a query selects, so each
 * newest comment's `state` is dropped unless the query's `newest` selection names it. */
function served(query: string, page: unknown): unknown {
  const newest = query.split("\n").find((line) => line.includes("newest:")) ?? "";
  if (page === undefined || /\bstate\b/.test(newest)) return page;
  return JSON.parse(JSON.stringify(page), (key, value) => (key === "state" ? undefined : value));
}

/** The daemon's gh-token answer naming both of Legion's role Apps by App role. */
const LEGION_APP_LOGINS = { implement: "legion-implementer[bot]", review: "legion-reviewer[bot]" };

/** A fake daemon + GitHub: `/legion/v1/gh-token` redeems the grant, naming `legionAppLogins` (none
 * when null); `api.github.com/graphql` serves `reviewThreads` pages keyed by the `after`
 * cursor ("null" for the first page) and records every `resolveReviewThread` mutation. */
function fakeGitHub(
  pages: Record<string, unknown>,
  mutation: (threadId: string) => unknown,
  tokenStatus = 200,
  legionAppLogins: Record<string, string> | null = LEGION_APP_LOGINS
) {
  const requests: Request[] = [];
  const graphqlBodies: Array<{ query: string; variables: Record<string, unknown> }> = [];
  const resolved: string[] = [];
  const fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const request = new Request(String(input), init);
    requests.push(request);
    if (new URL(request.url).pathname === "/legion/v1/gh-token") {
      if (tokenStatus !== 200) return new Response("nope", { status: tokenStatus });
      return Response.json({
        token: "scoped-token",
        appLogin: "legion-implementer[bot]",
        ...(legionAppLogins === null ? {} : { legionAppLogins }),
      });
    }
    const body = (await request.json()) as { query: string; variables: Record<string, unknown> };
    graphqlBodies.push(body);
    if (body.query.includes("resolveReviewThread")) {
      const threadId = body.variables.threadId as string;
      resolved.push(threadId);
      return Response.json(mutation(threadId));
    }
    return Response.json(served(body.query, pages[String(body.variables.after)]));
  };
  return { fetch, requests, graphqlBodies, resolved };
}

/** For a run that must never reach gh: the grant path, and `--gh` refused inside a pane. */
const noGh = {
  runGh: async (): Promise<never> => {
    throw new Error("gh ran");
  },
  stderr: (): never => {
    throw new Error("gh's stderr was written");
  },
};

/** A caller's own `gh` for `--gh`: `gh api graphql --input -` served from the same pages and
 * mutation as `fakeGitHub`, recording each call's argv and environment, and writing `stderr` on
 * every successful call. */
function fakeGh(
  pages: Record<string, unknown>,
  mutation: (threadId: string) => unknown,
  stderr = ""
) {
  const calls: Array<{ args: string[]; env: NodeJS.ProcessEnv }> = [];
  const resolved: string[] = [];
  const runGh = async (args: string[], stdin: string, env: NodeJS.ProcessEnv) => {
    calls.push({ args, env });
    const body = JSON.parse(stdin) as { query: string; variables: Record<string, unknown> };
    if (body.query.includes("resolveReviewThread")) {
      const threadId = body.variables.threadId as string;
      resolved.push(threadId);
      return { exitCode: 0, stdout: JSON.stringify(mutation(threadId)), stderr };
    }
    const page = served(body.query, pages[String(body.variables.after)]);
    return { exitCode: 0, stdout: JSON.stringify(page), stderr };
  };
  return { runGh, calls, resolved };
}

/** No daemon and no GitHub API from the CLI itself: `--gh` reaches GitHub only through `gh`. */
const noFetch = async (): Promise<never> => {
  throw new Error("--gh fetched");
};

describe("legion threads resolve", () => {
  it("resolves exactly the unresolved threads whose newest comment is the opener's Accepted: reply, across pages", async () => {
    const reviewer = "legion-reviewer";
    const github = fakeGitHub(
      {
        null: page(
          [
            thread("T1", 1, reviewer, {
              login: reviewer,
              body: "Accepted: fixed in abc1234 — guard added.",
            }),
            thread("T2", 2, reviewer, {
              login: reviewer,
              body: "Still open: the docstring still says env.",
            }),
          ],
          "CURSOR"
        ),
        CURSOR: page(
          [
            thread("T3", 3, reviewer, {
              login: "legion-implementer",
              body: "Accepted: fixed in abc1234",
            }),
            thread(
              "T4",
              4,
              reviewer,
              { login: reviewer, body: "Accepted: not a defect — by design." },
              { isResolved: true }
            ),
            thread("T5", 5, reviewer, {
              login: reviewer,
              body: "\n  Accepted: not a defect — by design.",
            }),
            thread("T6", 6, reviewer, {
              login: reviewer,
              body: "Accepted (round 2): fixed in abc1234",
            }),
          ],
          null
        ),
      },
      resolvedOk
    );
    const lines: string[] = [];

    await cmdThreadsResolve(
      { repo: "sjawhar/legion", pr: "993" },
      {
        env: { LEGION_GRANT: "grant-123" },
        fetch: github.fetch,
        ...noGh,
        log: (line) => lines.push(line),
      }
    );

    expect(github.resolved).toEqual(["T1", "T5"]);
    expect(lines).toEqual([
      `resolved ${PR}1 — its opener's acceptance`,
      `left open ${PR}2 — newest reply by legion-reviewer is not an acceptance`,
      `left open ${PR}3 — newest reply by legion-implementer is not an acceptance`,
      `resolved ${PR}5 — its opener's acceptance`,
      `left open ${PR}6 — newest reply by legion-reviewer is not an acceptance`,
    ]);
    const grant = github.requests[0] as Request;
    expect(new URL(grant.url).pathname).toBe("/legion/v1/gh-token");
    expect(await grant.json()).toEqual({ grantId: "grant-123" });
    for (const request of github.requests.slice(1)) {
      expect(request.url).toBe("https://api.github.com/graphql");
      expect(request.headers.get("authorization")).toBe("Bearer scoped-token");
    }
    const listCalls = github.graphqlBodies.filter((body) => body.query.includes("reviewThreads"));
    expect(listCalls.map((body) => body.variables.after)).toEqual([null, "CURSOR"]);
  });

  it("exits 1 naming the thread and GitHub's message when a resolve is refused, attempting nothing further", async () => {
    const reviewer = "legion-reviewer";
    const github = fakeGitHub(
      {
        null: page(
          [
            thread("T1", 1, reviewer, { login: reviewer, body: "Accepted: fixed in abc1234" }),
            thread("T2", 2, reviewer, { login: reviewer, body: "Accepted: fixed in abc1234" }),
          ],
          null
        ),
      },
      () => ({
        data: { resolveReviewThread: null },
        errors: [{ type: "FORBIDDEN", message: "Resource not accessible by integration" }],
      })
    );
    const lines: string[] = [];

    await expect(
      cmdThreadsResolve(
        { repo: "sjawhar/legion", pr: "993" },
        {
          env: { LEGION_GRANT: "grant-123" },
          fetch: github.fetch,
          ...noGh,
          log: (line) => lines.push(line),
        }
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message: `resolveReviewThread failed for ${PR}1: Resource not accessible by integration`,
        code: 1,
      })
    );
    expect(github.resolved).toEqual(["T1"]);
    expect(lines).toEqual([]);
  });

  it("fails before reaching GitHub when the grant cannot be redeemed", async () => {
    const github = fakeGitHub({}, resolvedOk, 403);

    await expect(
      cmdThreadsResolve(
        { repo: "sjawhar/legion", pr: "993" },
        {
          env: { LEGION_GRANT: "grant-123" },
          fetch: github.fetch,
          ...noGh,
          log: () => undefined,
        }
      )
    ).rejects.toEqual(
      // The CLI includes the status and daemon refusal message.
      expect.objectContaining({ message: "Unable to redeem LEGION_GRANT (403): nope", code: 1 })
    );
    expect(github.graphqlBodies).toEqual([]);
  });

  it("prints exactly `no unresolved threads` and mutates nothing when every thread is already resolved", async () => {
    const reviewer = "legion-reviewer";
    const github = fakeGitHub(
      {
        null: page(
          [
            thread(
              "T1",
              1,
              reviewer,
              { login: reviewer, body: "Accepted: fixed in abc1234" },
              { isResolved: true }
            ),
          ],
          null
        ),
      },
      resolvedOk
    );
    const lines: string[] = [];

    await cmdThreadsResolve(
      { repo: "sjawhar/legion", pr: "993" },
      {
        env: { LEGION_GRANT: "grant-123" },
        fetch: github.fetch,
        ...noGh,
        log: (line) => lines.push(line),
      }
    );

    expect(lines).toEqual(["no unresolved threads"]);
    expect(github.resolved).toEqual([]);
  });

  it("rejects a malformed --repo or --pr before redeeming any grant", async () => {
    for (const [options, message] of [
      [{ repo: "sjawhar", pr: "993" }, '--repo must be <owner>/<name> (got "sjawhar")'],
      [{ repo: "sjawhar/legion", pr: "abc" }, '--pr must be a pull request number (got "abc")'],
    ] as const) {
      const github = fakeGitHub({}, resolvedOk);

      await expect(
        cmdThreadsResolve(options, {
          env: { LEGION_GRANT: "grant-123" },
          fetch: github.fetch,
          ...noGh,
          log: () => undefined,
        })
      ).rejects.toEqual(expect.objectContaining({ message, code: 1 }));
      expect(github.requests).toEqual([]);
    }
  });

  it("--gh applies the same rule through the caller's gh, with no grant and from any directory", async () => {
    const reviewer = "legion-reviewer";
    const gh = fakeGh(
      {
        null: page(
          [
            thread("T1", 1, reviewer, { login: reviewer, body: "Accepted: fixed in abc1234" }),
            // An `Accepted:` from an account other than the opener is not the opener's
            // acceptance: refused.
            thread("T2", 2, reviewer, { login: "sjawhar-agent", body: "Accepted: fixed" }),
            thread("T3", 3, reviewer, { login: reviewer, body: "Still open: the test pins text." }),
          ],
          null
        ),
      },
      resolvedOk
    );
    const lines: string[] = [];
    const written: string[] = [];

    await cmdThreadsResolve(
      { repo: "sjawhar/legion", pr: "993", gh: true },
      {
        env: { OMP_SESSION_ID: "omp-session", GH_REPO: "acme/widgets" },
        fetch: noFetch,
        runGh: gh.runGh,
        log: (line) => lines.push(line),
        stderr: (text) => written.push(text),
      }
    );

    expect(gh.resolved).toEqual(["T1"]);
    expect(lines).toEqual([
      `resolved ${PR}1 — its opener's acceptance`,
      `left open ${PR}2 — newest reply by sjawhar-agent is not an acceptance`,
      `left open ${PR}3 — newest reply by legion-reviewer is not an acceptance`,
    ]);
    // A clean success writes nothing to stderr.
    expect(written).toEqual([]);
    // Every call gets the caller's environment, which decides the identity (the devbox shim routes
    // to an App only when it sees an agent session such as OMP_SESSION_ID), with GH_REPO set to
    // --repo over the caller's own: a routed gh outside a checkout has no other owner to route on.
    const env = { OMP_SESSION_ID: "omp-session", GH_REPO: "sjawhar/legion" };
    expect(gh.calls).toEqual([
      { args: ["api", "graphql", "--input", "-"], env },
      { args: ["api", "graphql", "--input", "-"], env },
    ]);
  });

  it("--gh shows gh's stderr from a successful call, where the devbox shim names an inherited GH_TOKEN the call then acts as", async () => {
    const reviewer = "legion-reviewer";
    const warning =
      "gh shim: GH_TOKEN inherited from the environment (ghp_…, a PERSONAL token: this call acts as its user, not as an App); not routing\n";
    const gh = fakeGh(
      {
        null: page(
          [thread("T1", 1, reviewer, { login: reviewer, body: "Accepted: fixed in abc1234" })],
          null
        ),
      },
      resolvedOk,
      warning
    );
    const lines: string[] = [];
    const written: string[] = [];

    await cmdThreadsResolve(
      { repo: "sjawhar/legion", pr: "993", gh: true },
      {
        env: { GH_TOKEN: "ghp_personal" },
        fetch: noFetch,
        runGh: gh.runGh,
        log: (line) => lines.push(line),
        stderr: (text) => written.push(text),
      }
    );

    expect(lines).toEqual([`resolved ${PR}1 — its opener's acceptance`]);
    // The query's and the mutation's, verbatim: the mutation acts as that token's owner.
    expect(written).toEqual([warning, warning]);
  });

  it("leaves open a thread whose newest comment is the opener's Accepted: still pending in an unsubmitted review, on both paths", async () => {
    // GitHub shows a pending review's drafts to their author only. Outside a pane the caller can
    // be the account that opened every thread (sjawhar-agent on sjawhar/*), and a role App can
    // have drafts of its own: neither path may act on one.
    const account = "sjawhar-agent";
    const pages = {
      null: page(
        [
          thread("T1", 1, account, {
            login: account,
            body: "Accepted: drafted, not yet submitted",
            state: "PENDING",
          }),
          thread("T2", 2, account, { login: account, body: "Accepted: fixed in abc1234" }),
        ],
        null
      ),
    };
    const github = fakeGitHub(pages, resolvedOk);
    const gh = fakeGh(pages, resolvedOk);
    const runs = [
      {
        gh: false,
        deps: { env: { LEGION_GRANT: "grant-123" }, fetch: github.fetch, ...noGh },
        resolved: github.resolved,
      },
      {
        gh: true,
        deps: { env: {}, fetch: noFetch, runGh: gh.runGh, stderr: () => undefined },
        resolved: gh.resolved,
      },
    ];

    for (const run of runs) {
      const lines: string[] = [];
      await cmdThreadsResolve(
        { repo: "sjawhar/legion", pr: "993", gh: run.gh },
        { ...run.deps, log: (line) => lines.push(line) }
      );

      expect(run.resolved).toEqual(["T2"]);
      expect(lines).toEqual([
        `left open ${PR}1 — newest reply by sjawhar-agent is an unsubmitted draft in a pending review`,
        `resolved ${PR}2 — its opener's acceptance`,
      ]);
    }
  });

  it("trims only space, tab, CR and LF before Accepted:, as the Go CLI does", async () => {
    // The shared vector with threads_test.go: both CLIs must answer these the same way.
    const reviewer = "legion-reviewer";
    const github = fakeGitHub(
      {
        null: page(
          [
            thread("T1", 1, reviewer, { login: reviewer, body: " \t\r\nAccepted: fixed" }),
            thread("T2", 2, reviewer, { login: reviewer, body: "\u00a0Accepted: fixed" }),
          ],
          null
        ),
      },
      resolvedOk
    );
    const lines: string[] = [];

    await cmdThreadsResolve(
      { repo: "sjawhar/legion", pr: "993" },
      {
        env: { LEGION_GRANT: "grant-123" },
        fetch: github.fetch,
        ...noGh,
        log: (line) => lines.push(line),
      }
    );

    expect(github.resolved).toEqual(["T1"]);
    expect(lines).toEqual([
      `resolved ${PR}1 — its opener's acceptance`,
      `left open ${PR}2 — newest reply by legion-reviewer is not an acceptance`,
    ]);
  });

  it("closes a bot's thread on the Legion reviewer's acceptance, never the author's reply, as the Go CLI does", async () => {
    // The shared vector with threads_test.go. The subject of a finding never closes it. A thread a
    // Bot opened that is none of Legion's role Apps (a CI bot, or a person whose gh is routed to an
    // App: GitHub cannot tell them apart) closes on its opener's Accepted:, or on Legion's review
    // App's, the independent party, and the resolved line says which. The pull request author's
    // reply (Fixed in, Declined) closes nothing. The review App's Accepted: counts only as the first
    // line of a submitted comment, after space, tab, CR or LF alone, whatever the login's case. A
    // thread either Legion App opened closes only on its opener's Accepted:, and a person's thread
    // is unchanged.
    const reviewer = "legion-reviewer";
    const author = "legion-implementer";
    const vectors: Array<[string, string, OpenerType, Comment]> = [
      [
        "reviewer-accepts",
        "claude",
        "Bot",
        { login: reviewer, body: "Accepted: fixed in 1a2b3c4 — moved the guard" },
      ],
      [
        "reviewer-accepts-any-case",
        "claude",
        "Bot",
        { login: "Legion-Reviewer", body: " \t\r\nAccepted: not a defect — the loop is bounded" },
      ],
      [
        "author-declined",
        "claude",
        "Bot",
        { login: author, body: "Declined: the loop is bounded" },
      ],
      [
        "author-fixed",
        "claude",
        "Bot",
        { login: author, body: "Fixed in 1a2b3c4: moved the guard" },
      ],
      ["author-accepts", "claude", "Bot", { login: author, body: "Accepted: my own fix" }],
      [
        "reviewer-still-open",
        "claude",
        "Bot",
        { login: reviewer, body: "Still open: the loop is not bounded" },
      ],
      ["reviewer-nbsp", "claude", "Bot", { login: reviewer, body: "\u00a0Accepted: fixed" }],
      [
        "reviewer-second-line",
        "claude",
        "Bot",
        { login: reviewer, body: "Thanks.\nAccepted: fixed" },
      ],
      [
        "reviewer-draft",
        "claude",
        "Bot",
        { login: reviewer, body: "Accepted: drafted", state: "PENDING" },
      ],
      [
        "routed-person",
        "sjawhar-agent",
        "Bot",
        { login: reviewer, body: "Accepted: fixed in 1a2b3c4 — moved the guard" },
      ],
      [
        "routed-person-own",
        "sjawhar-agent",
        "Bot",
        { login: "sjawhar-agent", body: "Accepted: fixed" },
      ],
      [
        "reviewer-thread",
        reviewer,
        "Bot",
        { login: author, body: "Fixed in 1a2b3c4: moved the guard" },
      ],
      ["implementer-app-thread", author, "Bot", { login: reviewer, body: "Accepted: fine" }],
      ["human", "octocat", "User", { login: reviewer, body: "Accepted: fixed" }],
    ];
    const github = fakeGitHub(
      {
        null: page(
          vectors.map(([id, opener, by, newest], n) => thread(id, n + 1, opener, newest, { by })),
          null
        ),
      },
      resolvedOk
    );
    const lines: string[] = [];

    await cmdThreadsResolve(
      { repo: "sjawhar/legion", pr: "993" },
      {
        env: { LEGION_GRANT: "grant-123" },
        fetch: github.fetch,
        ...noGh,
        log: (line) => lines.push(line),
      }
    );

    const byReviewer = "the Legion reviewer's acceptance of a bot's thread";
    const notEither = "not its opener's or the Legion reviewer's acceptance";
    expect(github.resolved).toEqual([
      "reviewer-accepts",
      "reviewer-accepts-any-case",
      "routed-person",
      "routed-person-own",
    ]);
    expect(lines).toEqual([
      `resolved ${PR}1 — ${byReviewer}`,
      `resolved ${PR}2 — ${byReviewer}`,
      `left open ${PR}3 — newest reply by ${author} is ${notEither}`,
      `left open ${PR}4 — newest reply by ${author} is ${notEither}`,
      `left open ${PR}5 — newest reply by ${author} is ${notEither}`,
      `left open ${PR}6 — newest reply by ${reviewer} is ${notEither}`,
      `left open ${PR}7 — newest reply by ${reviewer} is ${notEither}`,
      `left open ${PR}8 — newest reply by ${reviewer} is ${notEither}`,
      `left open ${PR}9 — newest reply by ${reviewer} is an unsubmitted draft in a pending review`,
      `resolved ${PR}10 — ${byReviewer}`,
      `resolved ${PR}11 — its opener's acceptance`,
      `left open ${PR}12 — newest reply by ${author} is not an acceptance`,
      `left open ${PR}13 — newest reply by ${reviewer} is not an acceptance`,
      `left open ${PR}14 — newest reply by ${reviewer} is not an acceptance`,
    ]);
  });

  it("applies no bot-thread rule when Legion's App logins are unknown: a daemon that names none, and --gh", async () => {
    // Which accounts are Legion's own, and which is its review App, is the daemon's to say, on the
    // grant's gh-token answer. A daemon that names none, and `--gh`,
    // which has no grant, leave every bot's thread to its opener's Accepted:, and the left-open line
    // says the session cannot tell rather than that the reply was not an acceptance.
    const pages = {
      null: page(
        [
          thread(
            "ci-bot",
            1,
            "claude",
            { login: "legion-reviewer", body: "Accepted: fixed" },
            { by: "Bot" }
          ),
          thread("human", 2, "octocat", {
            login: "legion-implementer",
            body: "Fixed in 1a2b3c4: moved the guard",
          }),
        ],
        null
      ),
    };
    const expected = [
      `left open ${PR}1 — newest reply by legion-reviewer is not its opener's acceptance, and this session cannot identify Legion's review App, so a bot's thread closes only on its opener's Accepted:`,
      `left open ${PR}2 — newest reply by legion-implementer is not an acceptance`,
    ];
    const github = fakeGitHub(pages, resolvedOk, 200, null);
    const daemonLines: string[] = [];
    await cmdThreadsResolve(
      { repo: "sjawhar/legion", pr: "993" },
      {
        env: { LEGION_GRANT: "grant-123" },
        fetch: github.fetch,
        ...noGh,
        log: (line) => daemonLines.push(line),
      }
    );
    expect(github.resolved).toEqual([]);
    expect(daemonLines).toEqual(expected);

    // A daemon names every App or none; an answer naming some is refused as invalid, as the Go CLI
    // refuses it.
    const partial = fakeGitHub(pages, resolvedOk, 200, { implement: "legion-implementer[bot]" });
    await expect(
      cmdThreadsResolve(
        { repo: "sjawhar/legion", pr: "993" },
        { env: { LEGION_GRANT: "grant-123" }, fetch: partial.fetch, ...noGh, log: () => undefined }
      )
    ).rejects.toThrow("Daemon returned an invalid GitHub credential response");
    expect(partial.resolved).toEqual([]);

    const gh = fakeGh(pages, resolvedOk);
    const ghLines: string[] = [];
    await cmdThreadsResolve(
      { repo: "sjawhar/legion", pr: "993", gh: true },
      {
        env: {},
        fetch: noFetch,
        runGh: gh.runGh,
        stderr: () => undefined,
        log: (line) => ghLines.push(line),
      }
    );
    expect(gh.resolved).toEqual([]);
    expect(ghLines).toEqual(expected);
  });

  it("refuses a newest comment that carries no state, resolving nothing", async () => {
    // GitHub always answers `state` when the query selects it; its absence means the query or the
    // response changed shape, and treating it as submitted would bring back resolve-on-a-draft.
    const reviewer = "legion-reviewer";
    const github = fakeGitHub(
      {
        null: page(
          [
            {
              id: "T1",
              isResolved: false,
              opener: { nodes: [{ url: `${PR}1`, author: { login: reviewer } }] },
              newest: { nodes: [{ author: { login: reviewer }, body: "Accepted: fixed" }] },
            },
          ],
          null
        ),
      },
      resolvedOk
    );

    await expect(
      cmdThreadsResolve(
        { repo: "sjawhar/legion", pr: "993" },
        { env: { LEGION_GRANT: "grant-123" }, fetch: github.fetch, ...noGh, log: () => undefined }
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message:
          'review thread T1: its newest comment carried state undefined, not "PENDING" or "SUBMITTED"',
        code: 1,
      })
    );
    expect(github.resolved).toEqual([]);
  });

  it("refuses an unresolved thread with no comments, as the Go CLI does", async () => {
    const github = fakeGitHub(
      {
        null: page(
          [{ id: "T1", isResolved: false, opener: { nodes: [] }, newest: { nodes: [] } }],
          null
        ),
      },
      resolvedOk
    );

    await expect(
      cmdThreadsResolve(
        { repo: "sjawhar/legion", pr: "993" },
        { env: { LEGION_GRANT: "grant-123" }, fetch: github.fetch, ...noGh, log: () => undefined }
      )
    ).rejects.toEqual(
      expect.objectContaining({ message: "review thread T1 has no comments", code: 1 })
    );
    expect(github.resolved).toEqual([]);
  });

  it("--gh exits 1 with gh's own message when gh fails, resolving nothing", async () => {
    const runGh = async () => ({
      exitCode: 4,
      stdout: "",
      stderr: "gh: To use GitHub CLI in automation, set the GH_TOKEN environment variable.\n",
    });
    const written: string[] = [];

    await expect(
      cmdThreadsResolve(
        { repo: "sjawhar/legion", pr: "993", gh: true },
        {
          env: {},
          fetch: noFetch,
          runGh,
          log: () => undefined,
          stderr: (text) => written.push(text),
        }
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message:
          "gh api graphql failed (exit 4): gh: To use GitHub CLI in automation, set the GH_TOKEN environment variable.",
        code: 1,
      })
    );
    // gh's message reaches the caller once, in the error.
    expect(written).toEqual([]);
  });

  it("--gh in a Legion pane is refused before gh runs, pointing at the grant path", async () => {
    // A pane's `gh` is `legion gh`, which refuses a GraphQL body it cannot read as a merge: the
    // pane's way to the rule is its grant, so --gh there is named as the wrong path up front.
    await expect(
      cmdThreadsResolve(
        { repo: "sjawhar/legion", pr: "993", gh: true },
        {
          env: { LEGION_GRANT_FILE: "/run/legion/grant" },
          fetch: noFetch,
          ...noGh,
          log: () => undefined,
        }
      )
    ).rejects.toEqual(
      expect.objectContaining({
        message:
          "--gh is for a session outside a Legion pane; this pane names a grant (LEGION_GRANT_FILE), so run legion threads resolve without --gh",
        code: 1,
      })
    );
  });
});

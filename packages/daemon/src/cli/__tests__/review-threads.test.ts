import { describe, expect, it } from "bun:test";
import { cmdThreadsResolve } from "../index";

const PR = "https://github.com/sjawhar/legion/pull/993#discussion_r";

interface Comment {
  login: string;
  body: string;
}

function thread(id: string, n: number, opener: string, newest: Comment, isResolved = false) {
  return {
    id,
    isResolved,
    opener: { nodes: [{ url: `${PR}${n}`, author: { login: opener } }] },
    newest: { nodes: [{ author: { login: newest.login }, body: newest.body }] },
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

/** A fake daemon + GitHub: `/legion/v1/gh-token` redeems the grant; `api.github.com/graphql`
 * serves `reviewThreads` pages keyed by the `after` cursor ("null" for the first page) and
 * records every `resolveReviewThread` mutation. */
function fakeGitHub(
  pages: Record<string, unknown>,
  mutation: (threadId: string) => unknown,
  tokenStatus = 200
) {
  const requests: Request[] = [];
  const graphqlBodies: Array<{ query: string; variables: Record<string, unknown> }> = [];
  const resolved: string[] = [];
  const fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const request = new Request(String(input), init);
    requests.push(request);
    if (new URL(request.url).pathname === "/legion/v1/gh-token") {
      if (tokenStatus !== 200) return new Response("nope", { status: tokenStatus });
      return Response.json({ token: "scoped-token", appLogin: "legion-implementer[bot]" });
    }
    const body = (await request.json()) as { query: string; variables: Record<string, unknown> };
    graphqlBodies.push(body);
    if (body.query.includes("resolveReviewThread")) {
      const threadId = body.variables.threadId as string;
      resolved.push(threadId);
      return Response.json(mutation(threadId));
    }
    return Response.json(pages[String(body.variables.after)]);
  };
  return { fetch, requests, graphqlBodies, resolved };
}

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
              true
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
      { env: { LEGION_GRANT: "grant-123" }, fetch: github.fetch, log: (line) => lines.push(line) }
    );

    expect(github.resolved).toEqual(["T1", "T5"]);
    expect(lines).toEqual([
      `resolved ${PR}1`,
      `left open ${PR}2 — newest reply by legion-reviewer is not an acceptance`,
      `left open ${PR}3 — newest reply by legion-implementer is not an acceptance`,
      `resolved ${PR}5`,
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
        { env: { LEGION_GRANT: "grant-123" }, fetch: github.fetch, log: (line) => lines.push(line) }
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
        { env: { LEGION_GRANT: "grant-123" }, fetch: github.fetch, log: () => undefined }
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
              true
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
      { env: { LEGION_GRANT: "grant-123" }, fetch: github.fetch, log: (line) => lines.push(line) }
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
          log: () => undefined,
        })
      ).rejects.toEqual(expect.objectContaining({ message, code: 1 }));
      expect(github.requests).toEqual([]);
    }
  });
});

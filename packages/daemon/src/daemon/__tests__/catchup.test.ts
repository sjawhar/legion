import { describe, expect, it, vi } from "bun:test";
import { type IssueKey, roleToken } from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import { type CatchupWorkerPayload, overseerCatchup, workerCatchup } from "../catchup";
import { TokenManager } from "../github-apps";
import { type LegionState, newLegionState, type PrState } from "../legion-state";

const WORKER_LOGIN = "legion-implement[bot]";
const WORKER_EMAIL = "123+legion-implement[bot]@users.noreply.github.com";

function stateForTree(): {
  state: LegionState;
  root: IssueKey;
  child: IssueKey;
} {
  const state = newLegionState("omp", 2);
  const root = "WIDGETS-1" as IssueKey;
  const child = "WIDGETS-2" as IssueKey;
  state.issues[root] = {
    key: root,
    title: "Root",
    status: "in_progress",
    children: [child],
  };
  state.issues[child] = {
    key: child,
    title: "Child",
    status: "done",
    parent: root,
    children: [],
  };
  state.trees[root] = {
    root,
    generation: 1,
    status: "active",
    launchFailures: 0,
  };
  return { state, root, child };
}

function prState(issue: IssueKey): PrState {
  return {
    key: issue,
    repo: "acme/widgets",
    number: 7,
    headSha: "head-7",
    verdict: "green",
    failing: [],
    failingStatuses: [],
    ciSettledAt: 3_000,
    ciCheckRuns: [{ name: "build", id: 3 }],
    ciSettlementGeneration: null,
    ciSnapshot: null,
    ciReconciled: false,
    fixAttempts: 1,
    reviewDecision: "approved",
  };
}

function tokenManager(): TokenManager {
  const manager = new TokenManager({});
  manager.getToken = async () => ({
    token: "ghs_catchup",
    expiresAt: "2099-01-01T00:00:00.000Z",
    gitIdentity: { name: WORKER_LOGIN, email: WORKER_EMAIL },
  });
  return manager;
}

function timelineRunner(timeline: {
  commits: unknown[];
  comments: unknown[];
  reviewComments: unknown[];
  reviews: unknown[];
}): { runner: CommandRunner; calls: string[][] } {
  const calls: string[][] = [];
  const runner: CommandRunner = async (command, options) => {
    calls.push(command);
    expect(options?.env?.GH_TOKEN).toBe("ghs_catchup");
    const endpoint = command[command.length - 1];
    const body =
      endpoint.includes("/pulls/") && endpoint.endsWith("/comments")
        ? timeline.reviewComments
        : endpoint.endsWith("/commits")
          ? timeline.commits
          : endpoint.endsWith("/comments")
            ? timeline.comments
            : timeline.reviews;
    const pages = Array.isArray(body[0]) ? body : [body];
    return { stdout: JSON.stringify(pages), stderr: "", exitCode: 0 };
  };
  return { runner, calls };
}

describe("derived catch-up", () => {
  it("derives an overseer snapshot from tree state", async () => {
    const { state, root, child } = stateForTree();
    state.prs["acme/widgets#7"] = prState(root);
    // Approved at 2 but edited since (latest 3): the snapshot must say closed, not just echo fields.
    state.gates[root] = { artifactId: "art-root", latestVersion: 3, approvedVersion: 2 };
    state.gates[child] = { artifactId: "art-child", latestVersion: 2, approvedVersion: 2 };

    expect(await overseerCatchup(state, root)).toEqual({
      type: "catchup-overseer",
      gates: {
        [root]: { artifactId: "art-root", latestVersion: 3, approvedVersion: 2, open: false },
        [child]: { artifactId: "art-child", latestVersion: 2, approvedVersion: 2, open: true },
      },
      childCounts: {
        [root]: { total: 1, open: 0, closed: 1 },
        [child]: { total: 0, open: 0, closed: 0 },
      },
      prVerdicts: {
        "acme/widgets#7": {
          issue: root,
          sha: "head-7",
          ci: "green",
          review: "approved",
          fixAttempts: 1,
        },
      },
      phaseCompletions: [],
    });
  });

  it("includes a phase that finished with no live architect holder, but not one still awaiting completion", async () => {
    const { state, root, child } = stateForTree();
    state.phases[root] = {
      phase: "tester",
      sessionId: "ses_tester",
      completed: { summary: "Verified the acceptance criteria", at: "2026-09-09T00:00:00.000Z" },
    };
    state.phases[child] = { phase: "implementer", sessionId: "ses_implementer" };

    expect(await overseerCatchup(state, root)).toMatchObject({
      phaseCompletions: [
        {
          issue: root,
          role: "tester",
          summary: "Verified the acceptance criteria",
          at: "2026-09-09T00:00:00.000Z",
        },
      ],
    });
  });

  it("lists a child's recorded completion on its sub-architect's snapshot, not the root's, once the child holds an architect claim", async () => {
    const { state, root, child } = stateForTree();
    state.issues[child].status = "in_progress";
    const childPlanner = {
      issue: child,
      role: "planner",
      summary: "Planned the child",
      at: "2026-09-13T00:00:00.000Z",
    };
    const rootTester = {
      issue: root,
      role: "tester",
      summary: "Verified",
      at: "2026-09-13T00:01:00.000Z",
    };
    state.phases[child] = {
      phase: "planner",
      sessionId: "ses_planner",
      completed: { summary: childPlanner.summary, at: childPlanner.at },
    };
    state.phases[root] = {
      phase: "tester",
      sessionId: "ses_tester",
      completed: { summary: rootTester.summary, at: rootTester.at },
    };

    // No claim on the child: the root owns both completions.
    expect((await overseerCatchup(state, root)).phaseCompletions).toEqual([
      rootTester,
      childPlanner,
    ]);

    state.roles[roleToken("omp", child, "architect")] = {
      issue: child,
      role: "architect",
      sessionId: "ses_sub",
    };
    expect((await overseerCatchup(state, root)).phaseCompletions).toEqual([rootTester]);
    expect((await overseerCatchup(state, child)).phaseCompletions).toEqual([childPlanner]);

    // A completed sub-architect phase belongs to the architect above it: the root's snapshot.
    const subArchitectDone = {
      issue: child,
      role: "architect",
      summary: "Child tree done",
      at: "2026-09-13T00:02:00.000Z",
    };
    state.phases[child] = {
      phase: "architect",
      sessionId: "ses_sub",
      completed: { summary: subArchitectDone.summary, at: subArchitectDone.at },
    };
    expect((await overseerCatchup(state, root)).phaseCompletions).toEqual([
      rootTester,
      subArchitectDone,
    ]);
    expect((await overseerCatchup(state, child)).phaseCompletions).toEqual([]);
  });

  it("includes failed check names in a red CI catch-up verdict", async () => {
    const { state, root } = stateForTree();
    state.prs["acme/widgets#7"] = {
      ...prState(root),
      verdict: "red",
      failing: ["lint", "unit"],
    };

    expect(await overseerCatchup(state, root)).toMatchObject({
      prVerdicts: {
        "acme/widgets#7": {
          ci: "red",
          failing: ["lint", "unit"],
        },
      },
    });
  });

  it("returns only human activity newer than the worker's own last commit", async () => {
    const { state, root } = stateForTree();
    state.prs["acme/widgets#7"] = prState(root);
    const { runner, calls } = timelineRunner({
      commits: [
        {
          commit: {
            author: { email: WORKER_EMAIL, date: "2026-08-24T10:02:00Z" },
          },
        },
      ],
      comments: [
        [
          {
            id: 1,
            created_at: "2026-08-24T10:01:00Z",
            user: { login: "human" },
            body: "T1",
            html_url: "https://example.test/comments/1",
          },
          {
            id: 2,
            created_at: "2026-08-24T10:02:00Z",
            user: { login: WORKER_LOGIN },
            body: "T2",
            html_url: "https://example.test/comments/2",
          },
        ],
        [
          {
            id: 3,
            created_at: "2026-08-24T10:03:00Z",
            user: { login: "human" },
            body: "T3",
            html_url: "https://example.test/comments/3",
          },
        ],
      ],
      reviewComments: [],
      reviews: [],
    });

    expect(
      await workerCatchup(state, root, "implementer", {
        runner,
        baseEnv: {},
        tokenManager: tokenManager(),
        repo: "acme/widgets",
      })
    ).toEqual({
      type: "catchup-worker",
      unhandled: [
        {
          kind: "comment",
          id: 3,
          occurredAt: "2026-08-24T10:03:00Z",
          author: "human",
          body: "T3",
          url: "https://example.test/comments/3",
        },
      ],
    });
    expect(calls).toEqual([
      ["gh", "api", "--paginate", "--slurp", "repos/acme/widgets/pulls/7/commits"],
      ["gh", "api", "--paginate", "--slurp", "repos/acme/widgets/issues/7/comments"],
      ["gh", "api", "--paginate", "--slurp", "repos/acme/widgets/pulls/7/comments"],
      ["gh", "api", "--paginate", "--slurp", "repos/acme/widgets/pulls/7/reviews"],
    ]);
  });

  it("includes post-cursor human reviews and inline feedback while excluding bot activity", async () => {
    const { state, root } = stateForTree();
    state.prs["acme/widgets#7"] = prState(root);
    const { runner } = timelineRunner({
      commits: [
        {
          commit: {
            author: { email: WORKER_EMAIL, date: "2026-08-24T10:00:00Z" },
          },
        },
      ],
      comments: [
        {
          id: 4,
          created_at: "2026-08-24T10:01:00Z",
          user: { login: "other-bot[bot]" },
          body: "ignore",
          html_url: "https://example.test/comments/4",
        },
      ],
      reviewComments: [
        {
          id: 7,
          created_at: "2026-08-24T10:01:00Z",
          user: { login: "reviewer" },
          body: "Inline feedback.",
          html_url: "https://example.test/review-comments/7",
        },
      ],
      reviews: [
        {
          id: 5,
          submitted_at: "2026-08-24T10:02:00Z",
          user: { login: "reviewer" },
          state: "CHANGES_REQUESTED",
          body: "Please fix this.",
          html_url: "https://example.test/reviews/5",
        },
        {
          id: 6,
          submitted_at: "2026-08-24T10:03:00Z",
          user: { login: WORKER_LOGIN },
          state: "APPROVED",
          body: "self",
          html_url: "https://example.test/reviews/6",
        },
      ],
    });

    expect(
      await workerCatchup(state, root, "implementer", {
        runner,
        baseEnv: {},
        tokenManager: tokenManager(),
        repo: "acme/widgets",
      })
    ).toEqual({
      type: "catchup-worker",
      unhandled: [
        {
          kind: "review-comment",
          id: 7,
          occurredAt: "2026-08-24T10:01:00Z",
          author: "reviewer",
          body: "Inline feedback.",
          url: "https://example.test/review-comments/7",
        },
        {
          kind: "review",
          id: 5,
          occurredAt: "2026-08-24T10:02:00Z",
          author: "reviewer",
          state: "changes_requested",
          body: "Please fix this.",
          url: "https://example.test/reviews/5",
        },
      ],
    });
  });

  it("never throws when a gh read fails: the catch-up is sent with the failure named beside what was gathered, logged once (LEGION-179)", async () => {
    const { state, root } = stateForTree();
    state.prs["acme/widgets#7"] = prState(root);
    state.prs["acme/widgets#8"] = { ...prState(root), number: 8 };
    const { runner: healthy } = timelineRunner({
      commits: [],
      comments: [
        {
          id: 1,
          created_at: "2026-08-24T10:01:00Z",
          user: { login: "human" },
          body: "T1",
          html_url: "https://example.test/comments/1",
        },
      ],
      reviewComments: [],
      reviews: [],
    });
    // Pull request 7 reads fine; the first read of pull request 8 fails as an unreachable GitHub
    // does (`gh` exits non-zero).
    const runner: CommandRunner = async (command, options) => {
      if (command[command.length - 1]?.includes("/8/")) {
        return { stdout: "", stderr: "connect: network is unreachable", exitCode: 1 };
      }
      return healthy(command, options);
    };
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let payload: CatchupWorkerPayload;
    let logged: string[];
    try {
      payload = await workerCatchup(state, root, "implementer", {
        runner,
        baseEnv: {},
        tokenManager: tokenManager(),
        repo: "acme/widgets",
      });
      logged = errors.mock.calls.map((call) => call.map(String).join(" "));
    } finally {
      errors.mockRestore();
    }

    expect(payload).toEqual({
      type: "catchup-worker",
      unhandled: [
        {
          kind: "comment",
          id: 1,
          occurredAt: "2026-08-24T10:01:00Z",
          author: "human",
          body: "T1",
          url: "https://example.test/comments/1",
        },
      ],
      github: {
        error: "acme/widgets#8: GitHub catch-up query failed: connect: network is unreachable",
      },
    });
    expect(logged).toEqual([
      `[legion] WIDGETS-1/implementer catch-up: GitHub enrichment failed (acme/widgets#8: GitHub catch-up query failed: connect: network is unreachable); sending the state-derived catch-up without it — the resumed worker reads GitHub itself`,
    ]);
  });

  it("never throws when the App token cannot be minted: the catch-up is sent with no GitHub activity and the failure named (LEGION-179)", async () => {
    const { state, root } = stateForTree();
    state.prs["acme/widgets#7"] = prState(root);
    const manager = new TokenManager({});
    manager.getToken = async () => {
      throw new Error("GitHub App token request failed: 503");
    };
    let reads = 0;
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    let payload: CatchupWorkerPayload;
    try {
      payload = await workerCatchup(state, root, "tester", {
        runner: async () => {
          reads += 1;
          return { stdout: "[]", stderr: "", exitCode: 0 };
        },
        baseEnv: {},
        tokenManager: manager,
        repo: "acme/widgets",
      });
    } finally {
      errors.mockRestore();
    }

    expect(payload).toEqual({
      type: "catchup-worker",
      unhandled: [],
      github: { error: "GitHub App token request failed: 503" },
    });
    expect(reads).toBe(0);
  });
});

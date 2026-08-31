import { describe, expect, it } from "bun:test";
import { formatIssueKey, type IssueKey } from "@legion/contracts";
import type { CommandRunner } from "../../state/fetch";
import { overseerCatchup, workerCatchup } from "../catchup";
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
  const root = formatIssueKey("acme", "widgets", 1);
  const child = formatIssueKey("acme", "widgets", 2);
  state.issues[root] = {
    key: root,
    title: "Root",
    state: "open",
    children: [child],
    released: true,
    labels: ["needs-approval"],
  };
  state.issues[child] = {
    key: child,
    title: "Child",
    state: "closed",
    parent: root,
    children: [],
    released: true,
    labels: ["human-approved", "legion-child"],
  };
  state.trees[root] = {
    root,
    generation: 1,
    status: "active",
    launchFailures: 0,
    heldEvents: [],
  };
  return { state, root, child };
}

function prState(issue: IssueKey): PrState {
  return {
    key: issue,
    repo: "acme/widgets",
    number: 7,
    headSha: "head-7",
    checks: {
      build: { status: "completed", conclusion: "success" },
      unit: { status: "completed", conclusion: "success" },
    },
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: true,
    lastEventAt: 3_000,
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
  it("derives an overseer snapshot from tree state without replaying stored events", async () => {
    const { state, root, child } = stateForTree();
    state.prs["acme/widgets#7"] = prState(root);
    state.trees[root].heldEvents.push({
      role: "ignored",
      payloadJson: JSON.stringify({ type: "old-event" }),
      heldAt: "2026-08-24T00:00:00.000Z",
      eventId: "old-event",
    });

    expect(await overseerCatchup(state, root)).toEqual({
      type: "catchup-overseer",
      gates: {
        [root]: { needsApproval: true, humanApproved: false },
        [child]: { needsApproval: false, humanApproved: true },
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
        tokenManager: tokenManager(),
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
        tokenManager: tokenManager(),
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
});

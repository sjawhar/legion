import { describe, expect, it } from "bun:test";
import { formatIssueKey, type IssueKey, roleToken } from "@legion/contracts";
import { type LegionState, newLegionState, type PrState } from "../legion-state";
import {
  type Effect,
  type EnvelopeJson,
  type ReducerConfig,
  reduceCiEmission,
  reduceGithubEvent,
} from "../reducers";

const repo = "acme/widgets" as const;
const root = formatIssueKey("acme", "widgets", 1);
const child = formatIssueKey("acme", "widgets", 2);
const prNumber = 17;
const config: ReducerConfig = {
  boardProjectIds: ["PVT_board"],
  appLogins: ["legion-author[bot]", "legion-reviewer[bot]"],
  maxFixAttempts: 3,
};

function envelope(payload: Record<string, unknown>, eventId = "delivery-1"): EnvelopeJson {
  return {
    event_id: eventId,
    issued_at: 1_700_000_000_000,
    payload,
  };
}

function issue(number: number, overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    number,
    title: `Issue ${number}`,
    state: "open",
    html_url: `https://github.com/${repo}/issues/${number}`,
    labels: [],
    ...overrides,
  };
}

function github(payload: Record<string, unknown>, eventId?: string): EnvelopeJson {
  return envelope({ repository: { full_name: repo }, ...payload }, eventId);
}

function rootState(status: "active" | "lingering" | "closed" = "active"): LegionState {
  const state = newLegionState("omp", 4);
  state.issues[root] = {
    key: root,
    title: "Root",
    state: "open",
    children: [],
    released: true,
    labels: [],
  };
  state.trees[root] = {
    root,
    generation: 1,
    status,
    heldEvents: [],
    launchFailures: 0,
  };
  claim(state, root, "architect");
  return state;
}

function claim(state: LegionState, key: IssueKey, role: "architect" | "implementer"): string {
  const token = roleToken(state.project, key, role);
  state.roles[token] = { issue: key, role };
  return token;
}

function attachChild(state: LegionState, released = true): void {
  state.issues[root].children.push(child);
  state.issues[child] = {
    key: child,
    title: "Child",
    state: "open",
    parent: root,
    children: [],
    released,
    labels: [],
  };
}

function addPr(state: LegionState, overrides: Partial<PrState> = {}): void {
  state.prs[`${repo}#${prNumber}`] = {
    key: child,
    repo,
    number: prNumber,
    headSha: "old-sha",
    checks: {},
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: false,
    lastEventAt: 0,
    fixAttempts: 0,
    ...overrides,
  };
  state.prByBranch[`${repo}@legion/issue-2`] = `${repo}#${prNumber}`;
}

function effects(
  state: LegionState,
  payload: Record<string, unknown>,
  topic = `notifications.github.acme.widgets.issue.1`,
  eventId?: string
): Effect[] {
  return reduceGithubEvent(state, topic, github(payload, eventId), config);
}

describe("reduceGithubEvent", () => {
  it("creates a board issue and wakes the controller with its preexisting children", () => {
    const state = newLegionState("omp", 4);
    const result = effects(state, {
      action: "opened",
      project: { id: "PVT_board" },
      issue: issue(1, { sub_issues: [issue(2)] }),
    });

    expect(state.issues[root]).toMatchObject({
      key: root,
      state: "open",
      children: [child],
    });
    expect(state.issues[child]).toMatchObject({
      key: child,
      parent: root,
      state: "open",
      released: false,
    });
    expect(result).toEqual([
      {
        kind: "controller",
        payload: {
          type: "triage",
          issue: root,
          preexistingChildren: [child],
        },
      },
    ]);

    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      heldEvents: [],
      launchFailures: 0,
    };
    const architect = claim(state, root, "architect");
    expect(
      effects(state, {
        action: "closed",
        issue: issue(2, { state: "closed" }),
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: {
          type: "child-closed",
          child,
          completion: "closed",
          remaining: 0,
          finalCommentRef: null,
        },
      },
      {
        kind: "publish",
        role: architect,
        payload: { type: "children-complete" },
      },
    ]);
  });

  it("excludes dispatch-thread children from ingress adoption", () => {
    const state = newLegionState("omp", 4);
    const excludedChild = formatIssueKey("acme", "widgets", 3);
    const result = effects(state, {
      action: "opened",
      project: { id: "PVT_board" },
      issue: issue(1, {
        sub_issues: [issue(2), issue(3, { labels: ["dispatch-thread"] })],
      }),
    });

    expect(state.issues[root]).toMatchObject({ children: [child] });
    expect(state.issues[child]).toBeDefined();
    expect(state.issues[excludedChild]).toBeUndefined();
    expect(result).toEqual([
      {
        kind: "controller",
        payload: { type: "triage", issue: root, preexistingChildren: [child] },
      },
    ]);
  });

  it("creates an issue from a board item-created event", () => {
    const state = newLegionState("omp", 4);
    const result = effects(state, {
      action: "created",
      project: { id: "PVT_board" },
      projects_v2_item: { content: issue(1) },
    });

    expect(state.issues[root]).toMatchObject({ key: root, title: "Issue 1" });
    expect(result).toEqual([
      {
        kind: "controller",
        payload: { type: "triage", issue: root, preexistingChildren: [] },
      },
    ]);
  });

  it("does not triage a dispatch thread added to the board", () => {
    const state = newLegionState("omp", 4);

    expect(
      effects(state, {
        action: "created",
        project: { id: "PVT_board" },
        projects_v2_item: { content: issue(1, { labels: [{ name: "dispatch-thread" }] }) },
      })
    ).toEqual([]);
    expect(state.issues[root]).toBeUndefined();
  });

  it("does not triage child or backlog issue ingress", () => {
    for (const labels of [["legion-child"], ["legion-backlog"]]) {
      const state = newLegionState("omp", 4);
      expect(
        effects(state, {
          action: "opened",
          project: { id: "PVT_board" },
          issue: issue(1, { labels }),
        })
      ).toEqual([]);
      expect(state.issues[root]).toBeUndefined();
    }
  });

  it("adopts a human-added child on an active tree but ignores an already-recorded legion child", () => {
    const state = rootState();
    const architect = roleToken(state.project, root, "architect");

    expect(
      effects(state, {
        action: "sub_issue_added",
        parent_issue: issue(1),
        sub_issue: issue(2),
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "child-adopted", child, remaining: 1 },
      },
    ]);
    expect(state.issues[root].children).toEqual([child]);
    expect(state.issues[child]).toMatchObject({
      parent: root,
      released: false,
    });

    expect(
      effects(state, {
        action: "sub_issue_added",
        parent_issue: issue(1),
        sub_issue: issue(2),
      })
    ).toEqual([]);
  });

  it("never adopts a dispatch thread as a child", () => {
    const state = rootState();

    expect(
      effects(state, {
        action: "sub_issue_added",
        parent_issue: issue(1),
        sub_issue: issue(2, { labels: ["dispatch-thread"] }),
      })
    ).toEqual([]);
    expect(state.issues[root].children).toEqual([]);
    expect(state.issues[child]).toBeUndefined();
  });

  it("reports child closure and the zero-crossing completion edge", () => {
    const state = rootState();
    attachChild(state);
    const architect = roleToken(state.project, root, "architect");

    expect(
      effects(state, {
        action: "closed",
        issue: issue(2, {
          state: "closed",
          state_reason: "completed",
          final_comment_ref: "comment-9",
        }),
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: {
          type: "child-closed",
          child,
          completion: "completed",
          remaining: 0,
          finalCommentRef: "comment-9",
        },
      },
      {
        kind: "publish",
        role: architect,
        payload: { type: "children-complete" },
      },
    ]);
    expect(state.issues[child].state).toBe("closed");
  });
  it("propagates a daemon-recorded closing comment when GitHub omits it from the close webhook", () => {
    const state = rootState();
    attachChild(state);
    const childNode = state.issues[child] as unknown as {
      finalCommentRef?: string;
    };
    childNode.finalCommentRef = "https://github.com/acme/widgets/issues/2#issuecomment-55";
    const architect = roleToken(state.project, root, "architect");

    expect(
      effects(state, {
        action: "closed",
        issue: issue(2, { state: "closed" }),
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: {
          type: "child-closed",
          child,
          completion: "closed",
          remaining: 0,
          finalCommentRef: "https://github.com/acme/widgets/issues/2#issuecomment-55",
        },
      },
      {
        kind: "publish",
        role: architect,
        payload: { type: "children-complete" },
      },
    ]);
  });

  it("re-arms children-complete after reopening a child and fires it on the next close", () => {
    const state = rootState();
    attachChild(state);
    const architect = roleToken(state.project, root, "architect");

    effects(state, { action: "closed", issue: issue(2, { state: "closed" }) });
    expect(effects(state, { action: "reopened", issue: issue(2) })).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "child-reopened", child },
      },
    ]);
    expect(
      effects(state, {
        action: "closed",
        issue: issue(2, { state: "closed" }),
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: {
          type: "child-closed",
          child,
          completion: "closed",
          remaining: 0,
          finalCommentRef: null,
        },
      },
      {
        kind: "publish",
        role: architect,
        payload: { type: "children-complete" },
      },
    ]);
  });

  it("reports a removed child and emits children-complete when it was the last open child", () => {
    const state = rootState();
    attachChild(state);
    const architect = roleToken(state.project, root, "architect");

    expect(
      effects(state, {
        action: "sub_issue_removed",
        parent_issue: issue(1),
        sub_issue: issue(2),
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "child-removed", child, remaining: 0 },
      },
      {
        kind: "publish",
        role: architect,
        payload: { type: "children-complete" },
      },
    ]);
    expect(state.issues[root].children).toEqual([]);
    expect(state.issues[child].parent).toBeUndefined();
  });

  it("routes a reopened removed child through controller triage instead of root resurrection", () => {
    const state = rootState();
    attachChild(state);
    effects(state, {
      action: "sub_issue_removed",
      parent_issue: issue(1),
      sub_issue: issue(2),
    });

    expect(effects(state, { action: "reopened", issue: issue(2) })).toEqual([
      {
        kind: "controller",
        payload: { type: "triage", issue: child, preexistingChildren: [] },
      },
    ]);
  });

  it("starts linger when a root of an active tree closes", () => {
    const state = rootState();
    expect(
      effects(state, {
        action: "closed",
        issue: issue(1, { state: "closed" }),
      })
    ).toEqual([{ kind: "linger", tree: root }]);
    expect(state.issues[root].state).toBe("closed");
  });

  it("ignores raw per-check observation topics", () => {
    const state = rootState();
    expect(
      effects(
        state,
        { action: "completed", check_run: { conclusion: "failure" } },
        `notifications.github.acme.widgets.pr.${prNumber}.check`
      )
    ).toEqual([]);
  });

  it("returns a lingering root to its architect on reopen", () => {
    const state = rootState("lingering");
    const architect = roleToken(state.project, root, "architect");

    expect(effects(state, { action: "reopened", issue: issue(1) })).toEqual([
      { kind: "publish", role: architect, payload: { type: "reopened" } },
    ]);
    expect(state.issues[root].state).toBe("open");
  });

  it("sends a gone root reopening through controller resurrection", () => {
    const state = rootState("closed");
    expect(effects(state, { action: "reopened", issue: issue(1) })).toEqual([
      { kind: "controller", payload: { type: "reactivation", issue: root } },
      { kind: "probe", tree: root },
    ]);
  });

  it("routes plain issue comments to the issue architect", () => {
    const state = rootState();
    const architect = roleToken(state.project, root, "architect");

    expect(
      effects(state, {
        action: "created",
        issue: issue(1),
        comment: {
          user: { login: "sami" },
          body: "Please adjust scope",
          html_url: "comment-url",
        },
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: {
          type: "issue-comment",
          author: "sami",
          body: "Please adjust scope",
          url: "comment-url",
        },
      },
    ]);
  });

  it("holds inactive child issue comments without a role claim", () => {
    const state = rootState();
    attachChild(state, false);
    const childArchitect = roleToken(state.project, child, "architect");
    expect(
      effects(
        state,
        {
          action: "created",
          issue: issue(2),
          comment: {
            user: { login: "sami" },
            body: "Hold this",
            html_url: "comment-url",
          },
        },
        undefined,
        "event-hold"
      )
    ).toEqual([
      {
        kind: "hold",
        tree: root,
        role: childArchitect,
        payload: {
          type: "issue-comment",
          author: "sami",
          body: "Hold this",
          url: "comment-url",
        },
      },
    ]);
    expect(state.trees[root].heldEvents).toEqual([
      {
        eventId: "event-hold",
        heldAt: "2023-11-14T22:13:20.000Z",
        role: childArchitect,
        payloadJson: JSON.stringify({
          type: "issue-comment",
          author: "sami",
          body: "Hold this",
          url: "comment-url",
        }),
      },
    ]);
  });

  it("filters legion-footer and self-authored comments before routing", () => {
    const state = rootState();
    const comment = {
      user: { login: "sami" },
      body: '<!-- legion: {"session":"x"} -->',
      html_url: "comment-url",
    };
    expect(effects(state, { action: "created", issue: issue(1), comment })).toEqual([]);
    expect(
      effects(state, {
        action: "created",
        issue: issue(1),
        comment: {
          ...comment,
          user: { login: "legion-author[bot]" },
          body: "Normal",
        },
      })
    ).toEqual([]);
  });

  it("routes PR conversation issue_comment events to the mapped implementer", () => {
    const state = rootState();
    attachChild(state);
    const implementer = claim(state, child, "implementer");
    addPr(state);

    expect(
      effects(state, {
        action: "created",
        issue: issue(prNumber, { pull_request: { url: "pr-api-url" } }),
        comment: {
          user: { login: "reviewer" },
          body: "Please rename this",
          html_url: "comment-url",
        },
      })
    ).toEqual([
      {
        kind: "publish",
        role: implementer,
        payload: {
          type: "pr-comment",
          author: "reviewer",
          body: "Please rename this",
          url: "comment-url",
        },
      },
    ]);
  });

  it("routes review comments to the mapped implementer and ignores unmapped PRs", () => {
    const state = rootState();
    attachChild(state);
    const implementer = claim(state, child, "implementer");
    addPr(state);
    const payload = {
      action: "created",
      pull_request: { number: prNumber },
      comment: {
        user: { login: "reviewer" },
        body: "Inline note",
        path: "src/reducers.ts",
        html_url: "comment-url",
      },
    };

    expect(effects(state, payload)).toEqual([
      {
        kind: "publish",
        role: implementer,
        payload: {
          type: "pr-review-comment",
          author: "reviewer",
          body: "Inline note",
          path: "src/reducers.ts",
          url: "comment-url",
        },
      },
    ]);
    expect(effects(rootState(), payload)).toEqual([]);
  });

  it("maps legion issue branches on PR opening and notifies the implementer", () => {
    const state = rootState();
    attachChild(state);
    const implementer = claim(state, child, "implementer");

    expect(
      effects(state, {
        action: "opened",
        pull_request: {
          number: prNumber,
          head: { ref: "legion/issue-2", sha: "head-sha" },
          html_url: "pr-url",
        },
      })
    ).toEqual([
      {
        kind: "publish",
        role: implementer,
        payload: { type: "pr-opened", pr: prNumber, url: "pr-url" },
      },
    ]);
    expect(state.prByBranch[`${repo}@legion/issue-2`]).toBe(`${repo}#${prNumber}`);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      key: child,
      headSha: "head-sha",
    });
  });
  it("registers a Legion PR on synchronization when its opened event was missed", () => {
    const state = rootState();
    attachChild(state);

    expect(
      effects(state, {
        action: "synchronize",
        pull_request: {
          number: prNumber,
          head: { ref: "legion/issue-2", sha: "recovered-head" },
        },
      })
    ).toEqual([{ kind: "approval-status", repo, pr: prNumber, sha: "recovered-head" }]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      key: child,
      headSha: "recovered-head",
    });
    expect(state.prByBranch[`${repo}@legion/issue-2`]).toBe(`${repo}#${prNumber}`);
  });

  it("resets PR checks and approval state on synchronization, counts a red-head retry, and rechecks approval", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, {
      headSha: "old-sha",
      checks: { tests: { status: "completed", conclusion: "failure" } },
      firstRedEmitted: true,
      settledRedEmitted: true,
      greenEmitted: true,
      reviewDecision: "approved",
    });

    expect(
      effects(state, {
        action: "synchronize",
        pull_request: {
          number: prNumber,
          head: { ref: "legion/issue-2", sha: "new-sha" },
        },
      })
    ).toEqual([{ kind: "approval-status", repo, pr: prNumber, sha: "new-sha" }]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      headSha: "new-sha",
      checks: {},
      firstRedEmitted: false,
      settledRedEmitted: false,
      greenEmitted: false,
      fixAttempts: 1,
    });
    expect(state.prs[`${repo}#${prNumber}`].reviewDecision).toBeUndefined();
  });

  it("treats neutral and skipped completed checks as green for an approved review", () => {
    const state = rootState();
    attachChild(state);
    const implementer = claim(state, child, "implementer");
    const architect = claim(state, child, "architect");
    addPr(state, {
      checks: {
        lint: { status: "completed", conclusion: "neutral" },
        optional: { status: "completed", conclusion: "skipped" },
      },
    });

    expect(
      effects(state, {
        action: "submitted",
        pull_request: { number: prNumber, head: { sha: "old-sha" } },
        review: {
          user: { login: "sami" },
          state: "approved",
          commit_id: "old-sha",
          body: "Looks good",
        },
      })
    ).toEqual([
      {
        kind: "publish",
        role: implementer,
        payload: {
          type: "pr-review",
          state: "approved",
          author: "sami",
          body: "Looks good",
        },
      },
      { kind: "approval-status", repo, pr: prNumber, sha: "old-sha" },
      {
        kind: "publish",
        role: architect,
        payload: { type: "pr-ready", pr: prNumber },
      },
    ]);
  });

  it("counts completed checks missing a conclusion as red when a PR synchronizes", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, {
      checks: { delayed: { status: "completed", conclusion: null } },
    });

    effects(state, {
      action: "synchronize",
      pull_request: {
        number: prNumber,
        head: { ref: "legion/issue-2", sha: "new-sha" },
      },
    });

    expect(state.prs[`${repo}#${prNumber}`].fixAttempts).toBe(1);
  });

  it("removes an unmerged PR mapping and tells the issue architect", () => {
    const state = rootState();
    attachChild(state);
    addPr(state);
    const architect = roleToken(state.project, child, "architect");
    claim(state, child, "architect");

    expect(
      effects(state, {
        action: "closed",
        pull_request: {
          number: prNumber,
          merged: false,
          head: { ref: "legion/issue-2" },
        },
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "pr-closed-unmerged", pr: prNumber },
      },
    ]);
    expect(state.prs[`${repo}#${prNumber}`]).toBeUndefined();
    expect(state.prByBranch[`${repo}@legion/issue-2`]).toBeUndefined();
  });

  it("records review state, notifies the implementer, emits approval-status, and notifies ready architect on a new green approval", () => {
    const state = rootState();
    attachChild(state);
    const implementer = claim(state, child, "implementer");
    const architect = claim(state, child, "architect");
    addPr(state, {
      checks: { tests: { status: "completed", conclusion: "success" } },
    });

    expect(
      effects(state, {
        action: "submitted",
        pull_request: { number: prNumber, head: { sha: "old-sha" } },
        review: {
          user: { login: "sami" },
          state: "approved",
          commit_id: "old-sha",
          body: "Looks good",
        },
      })
    ).toEqual([
      {
        kind: "publish",
        role: implementer,
        payload: {
          type: "pr-review",
          state: "approved",
          author: "sami",
          body: "Looks good",
        },
      },
      { kind: "approval-status", repo, pr: prNumber, sha: "old-sha" },
      {
        kind: "publish",
        role: architect,
        payload: { type: "pr-ready", pr: prNumber },
      },
    ]);
    expect(state.prs[`${repo}#${prNumber}`].reviewDecision).toBe("approved");
  });

  it("does not retain a review decision when the delivered review is pinned to a stale head", () => {
    const state = rootState();
    attachChild(state);
    const implementer = claim(state, child, "implementer");
    addPr(state, {
      headSha: "current-sha",
      checks: { tests: { status: "completed", conclusion: "success" } },
    });

    expect(
      effects(state, {
        action: "submitted",
        pull_request: { number: prNumber, head: { sha: "current-sha" } },
        review: {
          user: { login: "sami" },
          state: "approved",
          commit_id: "stale-sha",
          body: "Approved an earlier head",
        },
      })
    ).toEqual([
      {
        kind: "publish",
        role: implementer,
        payload: {
          type: "pr-review",
          state: "approved",
          author: "sami",
          body: "Approved an earlier head",
        },
      },
      { kind: "approval-status", repo, pr: prNumber, sha: "current-sha" },
    ]);
    expect(state.prs[`${repo}#${prNumber}`]?.reviewDecision).toBeUndefined();
  });

  it("mirrors supported labels, waking the architect once when a human approves", () => {
    const state = rootState();
    const architect = roleToken(state.project, root, "architect");
    expect(
      effects(state, {
        action: "labeled",
        issue: issue(1),
        label: { name: "needs-approval" },
      })
    ).toEqual([]);
    expect(state.issues[root].labels).toEqual(["needs-approval"]);
    expect(
      effects(state, {
        action: "labeled",
        issue: issue(1),
        label: { name: "human-approved" },
      })
    ).toEqual([{ kind: "publish", role: architect, payload: { type: "human-approved" } }]);
    expect(state.issues[root].labels).toEqual(["needs-approval", "human-approved"]);
    effects(state, {
      action: "labeled",
      issue: issue(1),
      label: { name: "unknown-label" },
    });
    expect(state.issues[root].labels).toEqual(["needs-approval", "human-approved"]);
    effects(state, {
      action: "unlabeled",
      issue: issue(1),
      label: { name: "needs-approval" },
    });
    expect(state.issues[root].labels).toEqual(["human-approved"]);
  });

  it("ignores pushes to legion issue branches", () => {
    const state = rootState();
    attachChild(state);

    expect(
      effects(state, {
        ref: "refs/heads/legion/issue-2",
        action: "labeled",
        issue: issue(1),
        label: { name: "human-approved" },
        commits: [{ id: "abc123", message: "Implement it" }],
      })
    ).toEqual([]);
    expect(state.issues[root].labels).toEqual([]);
  });
});

describe("reduceCiEmission", () => {
  it("notifies the architect when the CI edge turns green for an approved PR", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, { reviewDecision: "approved" });
    const architect = claim(state, child, "architect");

    expect(
      reduceCiEmission(state, repo, prNumber, { type: "ci-green", sha: "old-sha" }, config)
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "pr-ready", pr: prNumber },
      },
    ]);
  });

  it("notifies the architect when settled CI red exhausts the retry budget", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, { fixAttempts: 3 });
    const architect = claim(state, child, "architect");

    expect(
      reduceCiEmission(
        state,
        repo,
        prNumber,
        { type: "ci-settled-red", sha: "old-sha", failing: ["tests"] },
        config
      )
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "pr-blocked", pr: prNumber, attempts: 3 },
      },
    ]);
  });
});

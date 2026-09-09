import { describe, expect, it } from "bun:test";
import { formatIssueKey, type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import { type LegionState, newLegionState, type PrState } from "../legion-state";
import {
  type Effect,
  type EnvelopeJson,
  type ReducerConfig,
  reduceCiEmission,
  reduceGithubEvent,
  settleCiVerdict,
  uncertifyCiVerdict,
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
    updated_at: "2026-01-01T00:00:00.000Z",
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

function claim(state: LegionState, key: IssueKey, role: LegionRole): string {
  const token = roleToken(state.project, key, role);
  state.roles[token] = { issue: key, role };
  if (role !== "architect") {
    state.phases[key] = { phase: role, sessionId: `${role}-session` };
  }
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
  const {
    ciSettlementGeneration = null,
    ciSnapshot = null,
    ciReconciled = false,
    ...rest
  } = overrides;
  state.prs[`${repo}#${prNumber}`] = {
    key: child,
    repo,
    number: prNumber,
    headSha: "old-sha",
    verdict: null,
    failing: [],
    failingStatuses: [],
    ciSettledAt: null,
    ciCheckRuns: null,
    ciSettlementGeneration,
    ciSnapshot,
    ciReconciled,
    fixAttempts: 0,
    ...rest,
  };
  state.prByBranch[`${repo}@legion/issue-2`] = `${repo}#${prNumber}`;
}

function effects(
  state: LegionState,
  payload: Record<string, unknown>,
  topic = `notifications.github.acme.widgets.issue.1`,
  eventId?: string
): Effect[] {
  const input = payload.kind === "pr" ? envelope(payload, eventId) : github(payload, eventId);
  return reduceGithubEvent(state, topic, input, config);
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

  it("ignores a stale ingress redelivery whose issue.updated_at is older than a newer applied event", () => {
    const state = newLegionState("omp", 4);
    // A newer sub_issue_added event already advanced the root's fence past
    // the ingress payload's timestamp.
    state.issues[root] = {
      key: root,
      title: "Root",
      state: "open",
      children: [],
      released: true,
      labels: [],
      updatedAt: Date.parse("2026-01-01T00:00:02.000Z"),
    };

    const result = effects(state, {
      action: "opened",
      project: { id: "PVT_board" },
      issue: issue(1, {
        sub_issues: [issue(2)],
        updated_at: "2026-01-01T00:00:01.000Z",
      }),
    });

    expect(result).toEqual([]);
    // Nothing from the stale payload was applied: no children overwritten,
    // no tree created, fence untouched.
    expect(state.issues[root]).toMatchObject({
      children: [],
      updatedAt: Date.parse("2026-01-01T00:00:02.000Z"),
    });
    expect(state.trees[root]).toBeUndefined();
  });

  it("does not re-triage or overwrite state once a tree already exists for the ingress issue", () => {
    const state = newLegionState("omp", 4);
    const first = effects(state, {
      action: "opened",
      project: { id: "PVT_board" },
      issue: issue(1, { sub_issues: [issue(2)] }),
    });
    expect(first).toEqual([
      {
        kind: "controller",
        payload: { type: "triage", issue: root, preexistingChildren: [child] },
      },
    ]);
    state.trees[root] = {
      root,
      generation: 1,
      status: "active",
      heldEvents: [],
      launchFailures: 0,
    };
    // A newer event drops one of the children the first ingress recorded.
    state.issues[root].children = [];

    // A duplicate/redelivered ingress event, newer timestamp included, must
    // not re-triage or clobber the children a later event already changed:
    // the tree's existence means this issue was already triaged once.
    const second = effects(state, {
      action: "opened",
      project: { id: "PVT_board" },
      issue: issue(1, {
        sub_issues: [issue(2), issue(3)],
        updated_at: "2026-01-01T00:00:05.000Z",
      }),
    });

    expect(second).toEqual([]);
    expect(state.issues[root].children).toEqual([]);
  });

  it("preserves an issue's updatedAt fence across addNode when a later ingress event omits updated_at", () => {
    const state = newLegionState("omp", 4);
    state.issues[root] = {
      key: root,
      title: "Root",
      state: "open",
      children: [],
      released: true,
      labels: [],
      updatedAt: Date.parse("2026-01-01T00:00:05.000Z"),
    };

    // No tree yet, so this ingress isn't blocked by the tree-exists guard;
    // it omits updated_at entirely (unlike a real webhook, addNode must not
    // silently wipe the fence a prior event already established).
    effects(state, {
      action: "opened",
      project: { id: "PVT_board" },
      issue: issue(1, { updated_at: undefined }),
    });

    expect(state.issues[root].updatedAt).toBe(Date.parse("2026-01-01T00:00:05.000Z"));
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

  it("crashes loud on an issue event missing a parseable updated_at (a contract violation, not a real GitHub payload)", () => {
    const state = rootState();
    for (const updated_at of [undefined, "not-a-date"]) {
      expect(() =>
        effects(state, {
          action: "reopened",
          issue: issue(1, { updated_at }),
        })
      ).toThrow(/missing a parseable updated_at/);
    }
  });

  it("crashes loud on a sub_issue event missing a parseable parent_issue.updated_at", () => {
    const state = rootState();
    for (const updated_at of [undefined, "not-a-date"]) {
      expect(() =>
        effects(state, {
          action: "sub_issue_added",
          parent_issue: issue(1, { updated_at }),
          sub_issue: issue(2),
        })
      ).toThrow(/missing a parseable parent_issue.updated_at/);
    }
  });

  it("does not require updated_at on a resync-sourced issue or sub_issue event", () => {
    const state = rootState();
    // "resync" is the daemon's own sentinel topic for reducer input it
    // reconstructs from a board/CI read, not an external webhook — GitHub's
    // updated_at contract does not apply to it (see reduceGithubEvent).
    expect(() =>
      effects(state, { action: "reopened", issue: issue(1, { updated_at: undefined }) }, "resync")
    ).not.toThrow();
    expect(() =>
      effects(
        state,
        {
          action: "sub_issue_added",
          parent_issue: issue(1, { updated_at: undefined }),
          sub_issue: issue(2),
        },
        "resync"
      )
    ).not.toThrow();
  });

  it("does not let an issue action it ignores make itself the freshness authority", () => {
    const state = rootState();
    const architect = roleToken(state.project, root, "architect");

    // issueEvent recognizes labeled/unlabeled/closed/reopened only; "assigned"
    // falls through unhandled at a later timestamp than the approval below.
    expect(
      effects(state, {
        action: "assigned",
        issue: issue(1, { updated_at: "2026-01-01T00:00:02.000Z" }),
      })
    ).toEqual([]);
    expect(state.issues[root].updatedAt).toBeUndefined();

    // An earlier human-approved label must still apply: the ignored event
    // above never mutated anything, so it must not have become the fence.
    expect(
      effects(state, {
        action: "labeled",
        issue: issue(1, { updated_at: "2026-01-01T00:00:01.000Z" }),
        label: { name: "human-approved" },
      })
    ).toEqual([{ kind: "publish", role: architect, payload: { type: "human-approved" } }]);
    expect(state.issues[root].labels).toEqual(["human-approved"]);
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

  it("does not re-adopt a redelivered sub_issue_added that arrives after a newer sub_issue_removed", () => {
    const state = rootState();
    const architect = roleToken(state.project, root, "architect");
    const addedPayload = {
      action: "sub_issue_added",
      parent_issue: issue(1, { updated_at: "2026-01-01T00:00:01.000Z" }),
      sub_issue: issue(2),
    };

    expect(effects(state, addedPayload)).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "child-adopted", child, remaining: 1 },
      },
    ]);
    expect(state.issues[root].children).toEqual([child]);

    expect(
      effects(state, {
        action: "sub_issue_removed",
        parent_issue: issue(1, { updated_at: "2026-01-01T00:00:02.000Z" }),
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

    // Redelivery of the original sub_issue_added: its parent_issue.updated_at
    // (t=1) is now older than the parent's last-applied event (t=2, from the
    // removal above), so it is ignored instead of re-adopting the child.
    expect(effects(state, addedPayload)).toEqual([]);
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

  it("keeps settled CI envelopes out of the generic GitHub reducer", () => {
    const state = rootState();
    expect(
      effects(
        state,
        { action: "completed", check_run: { conclusion: "failure" } },
        `notifications.github.acme.widgets.pr.${prNumber}.checks`
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

  it("keeps routing to the active phase while a tree lingers", () => {
    const state = rootState("lingering");
    const implementer = claim(state, root, "implementer");

    expect(
      effects(state, {
        action: "created",
        issue: issue(1),
        comment: {
          user: { login: "sami" },
          body: "Any update?",
          html_url: "comment-url",
        },
      })
    ).toEqual([
      {
        kind: "publish",
        role: implementer,
        payload: {
          type: "issue-comment",
          author: "sami",
          body: "Any update?",
          url: "comment-url",
        },
      },
    ]);
  });

  it("wakes the controller for activity on a closed tree instead of publishing or holding", () => {
    const state = rootState("closed");

    expect(
      effects(state, {
        action: "created",
        issue: issue(1),
        comment: {
          user: { login: "sami" },
          body: "Still around?",
          html_url: "comment-url",
        },
      })
    ).toEqual([
      {
        kind: "controller",
        payload: {
          type: "closed-tree-activity",
          issue: root,
          root,
          event: {
            type: "issue-comment",
            author: "sami",
            body: "Still around?",
            url: "comment-url",
          },
        },
      },
    ]);
    expect(state.trees[root].heldEvents).toEqual([]);
  });

  it("wakes the controller exactly once for a closed tree's approved review at a green head", () => {
    const state = rootState("closed");
    attachChild(state);
    addPr(state, { verdict: "green", ciSettledAt: 0 });

    const result = effects(state, {
      action: "submitted",
      pull_request: { number: prNumber, head: { sha: "old-sha" } },
      review: {
        user: { login: "sami" },
        state: "approved",
        commit_id: "old-sha",
        body: "Looks good",
      },
    });

    expect(result.filter((effect) => effect.kind === "controller")).toEqual([
      {
        kind: "controller",
        payload: {
          type: "closed-tree-activity",
          issue: child,
          root,
          event: {
            type: "pr-review",
            state: "approved",
            author: "sami",
            body: "Looks good",
          },
        },
      },
    ]);
    expect(result).toContainEqual({ kind: "approval-status", repo, pr: prNumber, sha: "old-sha" });
  });

  it("wakes the controller exactly once when closing the last open child of a closed tree", () => {
    const state = rootState("closed");
    attachChild(state);

    expect(
      effects(state, {
        action: "closed",
        issue: issue(2, { state: "closed", state_reason: "completed" }),
      })
    ).toEqual([
      {
        kind: "controller",
        payload: {
          type: "closed-tree-activity",
          issue: root,
          root,
          event: {
            type: "child-closed",
            child,
            completion: "completed",
            remaining: 0,
            finalCommentRef: null,
          },
        },
      },
    ]);
  });

  it("routes comments on an unreleased child to the tree's architect and never holds them", () => {
    const state = rootState();
    attachChild(state, false);
    const architect = roleToken(state.project, root, "architect");
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
        kind: "publish",
        role: architect,
        payload: {
          type: "issue-comment",
          author: "sami",
          body: "Hold this",
          url: "comment-url",
        },
      },
    ]);
    expect(state.trees[root].heldEvents).toEqual([]);
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
        kind: "pr",
        action: "opened",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "head-sha",
        url: "pr-url",
        updated_at: "2026-09-07T03:00:00Z",
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
      headUpdatedAt: Date.parse("2026-09-07T03:00:00Z"),
    });
  });
  it("does not index a registered PR under an unknown branch", () => {
    const state = rootState();
    const fallbackIssue = formatIssueKey("acme", "widgets", prNumber);
    state.issues[fallbackIssue] = {
      key: fallbackIssue,
      title: "Fallback PR issue",
      state: "open",
      children: [],
      released: true,
      labels: [],
    };

    expect(
      effects(state, {
        kind: "pr",
        action: "opened",
        repo,
        number: String(prNumber),
        head_sha: "head-sha",
      })
    ).toEqual([]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      key: fallbackIssue,
      headSha: "head-sha",
    });
    expect(state.prByBranch).toEqual({});
  });

  it("registers a Legion PR on synchronization when its opened event was missed", () => {
    const state = rootState();
    attachChild(state);

    expect(
      effects(state, {
        kind: "pr",
        action: "synchronize",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "recovered-head",
      })
    ).toEqual([{ kind: "approval-status", repo, pr: prNumber, sha: "recovered-head" }]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      key: child,
      headSha: "recovered-head",
    });
    expect(state.prByBranch[`${repo}@legion/issue-2`]).toBe(`${repo}#${prNumber}`);
  });

  it("resets a red CI verdict and approval state on synchronization, counts the retry, and rechecks approval", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, {
      headSha: "old-sha",
      verdict: "red",
      failing: ["unit"],
      failingStatuses: [],
      ciSettledAt: 1,
      ciCheckRuns: [{ name: "build", id: 1 }],
      reviewDecision: "approved",
    });

    expect(
      effects(state, {
        kind: "pr",
        action: "synchronize",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "new-sha",
      })
    ).toEqual([{ kind: "approval-status", repo, pr: prNumber, sha: "new-sha" }]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      headSha: "new-sha",
      verdict: null,
      failing: [],
      failingStatuses: [],
      ciSettledAt: null,
      ciCheckRuns: null,
      fixAttempts: 1,
    });
    expect(state.prs[`${repo}#${prNumber}`].reviewDecision).toBeUndefined();
  });
  it("keeps a newer head and its verdict when synchronize arrives out of order", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, {
      headSha: "head-a",
      headUpdatedAt: Date.parse("2026-09-07T03:00:00Z"),
    });

    expect(
      effects(state, {
        kind: "pr",
        action: "synchronize",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "head-b",
        updated_at: "2026-09-07T03:02:00Z",
      })
    ).toEqual([{ kind: "approval-status", repo, pr: prNumber, sha: "head-b" }]);
    Object.assign(state.prs[`${repo}#${prNumber}`], {
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciSettledAt: 1,
      reviewDecision: "approved",
    });

    expect(
      effects(state, {
        kind: "pr",
        action: "synchronize",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "head-a",
        updated_at: "2026-09-07T03:01:00Z",
      })
    ).toEqual([]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      headSha: "head-b",
      headUpdatedAt: Date.parse("2026-09-07T03:02:00Z"),
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciSettledAt: 1,
      reviewDecision: "approved",
      fixAttempts: 0,
    });
  });

  it("notifies the active implementer of a review and the subsequent CI-ready signal", () => {
    const state = rootState();
    attachChild(state);
    const implementer = claim(state, child, "implementer");
    addPr(state, {
      verdict: "green",
      ciSettledAt: 0,
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
        role: implementer,
        payload: { type: "pr-ready", pr: prNumber },
      },
    ]);
    expect(state.prs[`${repo}#${prNumber}`].reviewDecision).toBe("approved");
  });

  it("falls back to the tree's architect for a review and its ready signal when no phase is active", () => {
    const state = rootState();
    attachChild(state);
    const architect = roleToken(state.project, root, "architect");
    addPr(state, {
      verdict: "green",
      ciSettledAt: 0,
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
        role: architect,
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

  it("removes an unmerged PR mapping and tells the tree's architect", () => {
    const state = rootState();
    attachChild(state);
    addPr(state);
    const architect = roleToken(state.project, root, "architect");

    expect(
      effects(state, {
        kind: "pr",
        action: "closed",
        repo,
        number: String(prNumber),
        merged: "false",
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

  it("keeps a tombstone after an unmerged close so an older opened redelivery cannot recreate the PR", () => {
    const state = rootState();
    attachChild(state);
    addPr(state);
    const architect = roleToken(state.project, root, "architect");

    expect(
      effects(state, {
        kind: "pr",
        action: "closed",
        repo,
        number: String(prNumber),
        merged: "false",
        updated_at: "2026-09-07T04:00:00Z",
      })
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "pr-closed-unmerged", pr: prNumber },
      },
    ]);
    expect(state.prs[`${repo}#${prNumber}`]).toBeUndefined();

    // An older "opened" redelivery (a stale duplicate webhook, or a
    // crash-before-ack redelivery of the original opened event) must not
    // resurrect a PR this state already recorded as closed.
    expect(
      effects(state, {
        kind: "pr",
        action: "opened",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "old-sha",
        updated_at: "2026-09-07T03:00:00Z",
      })
    ).toEqual([]);
    expect(state.prs[`${repo}#${prNumber}`]).toBeUndefined();
    expect(state.prByBranch[`${repo}@legion/issue-2`]).toBeUndefined();
  });

  it("keeps a tombstone after an unmerged close so an older synchronize cannot recreate the PR", () => {
    const state = rootState();
    attachChild(state);
    addPr(state);

    effects(state, {
      kind: "pr",
      action: "closed",
      repo,
      number: String(prNumber),
      merged: "false",
      updated_at: "2026-09-07T04:00:00Z",
    });
    expect(state.prs[`${repo}#${prNumber}`]).toBeUndefined();

    expect(
      effects(state, {
        kind: "pr",
        action: "synchronize",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "stale-resurrection-head",
        updated_at: "2026-09-07T03:30:00Z",
      })
    ).toEqual([]);
    expect(state.prs[`${repo}#${prNumber}`]).toBeUndefined();
  });

  it("allows a genuinely newer opened event to recreate a PR after an older tombstoned close", () => {
    const state = rootState();
    attachChild(state);
    addPr(state);
    const implementer = claim(state, child, "implementer");

    effects(state, {
      kind: "pr",
      action: "closed",
      repo,
      number: String(prNumber),
      merged: "false",
      updated_at: "2026-09-07T04:00:00Z",
    });

    expect(
      effects(state, {
        kind: "pr",
        action: "opened",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "reopened-head",
        url: "pr-url",
        updated_at: "2026-09-07T05:00:00Z",
      })
    ).toEqual([
      {
        kind: "publish",
        role: implementer,
        payload: { type: "pr-opened", pr: prNumber, url: "pr-url" },
      },
    ]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({ headSha: "reopened-head" });
  });

  it("ignores a stale opened event when a newer PR record already exists", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, { headUpdatedAt: Date.parse("2026-09-07T03:00:00Z") });

    expect(
      effects(state, {
        kind: "pr",
        action: "opened",
        repo,
        number: String(prNumber),
        head_ref: "legion/issue-2",
        head_sha: "stale-reopen-head",
        url: "pr-url",
        updated_at: "2026-09-07T02:00:00Z",
      })
    ).toEqual([]);
    // The existing PR record must survive untouched — a stale "opened"
    // redelivery must not reset it via registerPr.
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({ headSha: "old-sha" });
  });

  it("does not retain a review decision when the delivered review is pinned to a stale head", () => {
    const state = rootState();
    attachChild(state);
    const implementer = claim(state, child, "implementer");
    addPr(state, {
      headSha: "current-sha",
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
    addPr(state, { reviewDecision: "approved", verdict: "green", ciSettledAt: 0 });
    const architect = roleToken(state.project, root, "architect");

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
    addPr(state, { fixAttempts: 3, verdict: "red", ciSettledAt: 0 });
    const architect = roleToken(state.project, root, "architect");

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

describe("settleCiVerdict", () => {
  it("routes a CI settlement to the issue's active phase instead of a fixed role", () => {
    const state = rootState();
    attachChild(state);
    addPr(state);
    const tester = claim(state, child, "tester");

    expect(
      settleCiVerdict(
        state,
        state.prs[`${repo}#${prNumber}`],
        { verdict: "green", failing: [], failingStatuses: [], settledAt: 5 },
        config,
        envelope({})
      )
    ).toEqual([{ kind: "publish", role: tester, payload: { type: "ci-green", sha: "old-sha" } }]);
  });

  it("wakes the controller exactly once for a closed-tree settlement, even when a ready signal would otherwise derive", () => {
    const state = rootState("closed");
    attachChild(state);
    addPr(state, { reviewDecision: "approved" });

    expect(
      settleCiVerdict(
        state,
        state.prs[`${repo}#${prNumber}`],
        { verdict: "green", failing: [], failingStatuses: [], settledAt: 5 },
        config,
        envelope({})
      )
    ).toEqual([
      {
        kind: "controller",
        payload: {
          type: "closed-tree-activity",
          issue: child,
          root,
          event: { type: "ci-green", sha: "old-sha" },
        },
      },
    ]);
  });
});

describe("routeActive", () => {
  it("crashes loud on a persisted phase that names no recognized role", () => {
    const state = rootState();
    attachChild(state);
    state.phases[child] = { phase: "bogus", sessionId: "x" };

    expect(() =>
      effects(state, {
        action: "created",
        issue: issue(2),
        comment: { user: { login: "sami" }, body: "hi", html_url: "u" },
      })
    ).toThrow(child);
  });
});

describe("uncertifyCiVerdict", () => {
  it("clears a green verdict without changing CI settlement metadata", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, {
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
    });

    uncertifyCiVerdict(state.prs[`${repo}#${prNumber}`]);

    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      verdict: null,
      failing: [],
      failingStatuses: [],
      ciSettledAt: 1_000,
      ciCheckRuns: [{ name: "build", id: 4 }],
    });
  });
});

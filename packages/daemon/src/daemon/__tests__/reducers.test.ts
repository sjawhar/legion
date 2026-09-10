import { describe, expect, it } from "bun:test";
import { type IssueKey, type LegionRole, roleToken } from "@legion/contracts";
import { type IssueStatus, type LegionState, newLegionState, type PrState } from "../legion-state";
import {
  type DispatchIssueEvent,
  type Effect,
  type EnvelopeJson,
  type ReducerConfig,
  reduceCiEmission,
  reduceDispatchEvent,
  reduceGithubEvent,
  settleCiVerdict,
  uncertifyCiVerdict,
} from "../reducers";
import askAnswered from "./fixtures/dispatch/ask-answered.json";
import childStatus from "./fixtures/dispatch/child-status.json";
import issueClosed from "./fixtures/dispatch/issue-closed.json";
import issueCreatedChild from "./fixtures/dispatch/issue-created-child.json";
import issueCreatedRoot from "./fixtures/dispatch/issue-created-root.json";
import issueUpdatedBacklog from "./fixtures/dispatch/issue-updated-backlog.json";
import issueUpdatedIcebox from "./fixtures/dispatch/issue-updated-icebox.json";
import issueUpdatedInProgress from "./fixtures/dispatch/issue-updated-in-progress.json";
import issueUpdatedNeedsReview from "./fixtures/dispatch/issue-updated-needs-review.json";
import issueUpdatedRetro from "./fixtures/dispatch/issue-updated-retro.json";
import issueUpdatedTesting from "./fixtures/dispatch/issue-updated-testing.json";
import issueUpdatedTodo from "./fixtures/dispatch/issue-updated-todo.json";
import humanApproved from "./fixtures/dispatch/legsmoke-3-ask.answered-approve.json";
import humanTodo from "./fixtures/dispatch/legsmoke-3-issue.updated-human-todo.json";

const repo = "acme/widgets" as const;
const root = "LEGSMOKE-1" as IssueKey;
const child = "LEGSMOKE-2" as IssueKey;
const childBranch = `legion/${child}`;

const prNumber = 17;
const config: ReducerConfig = {
  appLogins: ["legion-author[bot]", "legion-reviewer[bot]"],
  maxFixAttempts: 3,
};

const DAEMON_STATUS_FIXTURES: ReadonlyArray<readonly [IssueStatus, DispatchFixture]> = [
  ["in_progress", issueUpdatedInProgress as unknown as DispatchFixture],
  ["testing", issueUpdatedTesting as unknown as DispatchFixture],
  ["needs_review", issueUpdatedNeedsReview as unknown as DispatchFixture],
  ["retro", issueUpdatedRetro as unknown as DispatchFixture],
];
const PARKED_STATUS_FIXTURES: ReadonlyArray<readonly [IssueStatus, DispatchFixture]> = [
  ["backlog", issueUpdatedBacklog as unknown as DispatchFixture],
  ["icebox", issueUpdatedIcebox as unknown as DispatchFixture],
];

interface DispatchFixture {
  readonly id: number;
  readonly issue_key: string;
  readonly seq: number;
  readonly notify: boolean;
  readonly type: DispatchIssueEvent["type"];
  readonly payload: unknown;
}

function dispatch(fixture: DispatchFixture): DispatchIssueEvent {
  return {
    type: fixture.type,
    key: fixture.issue_key as IssueKey,
    seq: fixture.seq,
    notify: fixture.notify,
    payload: fixture.payload,
    eventId: `dispatch-${fixture.id}`,
  };
}
function dispatchIssueWithKey(fixture: DispatchFixture, key: IssueKey): DispatchIssueEvent {
  const event = dispatch(fixture);
  if (typeof event.payload !== "object" || event.payload === null || Array.isArray(event.payload)) {
    throw new Error("issue.closed fixture payload must be an object");
  }
  return { ...event, key, payload: { ...event.payload, key } };
}

function envelope(payload: Record<string, unknown>, eventId = "delivery-1"): EnvelopeJson {
  return {
    event_id: eventId,
    issued_at: 1_700_000_000_000,
    payload,
  };
}

function github(payload: Record<string, unknown>, eventId?: string): EnvelopeJson {
  return envelope({ repository: { full_name: repo }, ...payload }, eventId);
}
function issue(number: number, overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    number,
    title: `Issue ${number}`,
    state: "open",
    updated_at: "2026-01-01T00:00:00.000Z",
    ...overrides,
  };
}

function issueNode(
  key: IssueKey,
  title: string,
  status: IssueStatus = "triage",
  parent?: IssueKey
) {
  return {
    key,
    title,
    state: status === "done" ? ("closed" as const) : ("open" as const),
    ...(parent === undefined ? {} : { parent }),
    children: [],
    released: true,
    labels: [],
    status,
  };
}

function rootState(status: "active" | "lingering" | "closed" = "active"): LegionState {
  const state = newLegionState("omp", 4);
  state.issues[root] = issueNode(root, "Root");
  state.trees[root] = {
    root,
    generation: 1,
    status,
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

function attachChild(state: LegionState): void {
  state.issues[root].children.push(child);
  state.issues[child] = issueNode(child, "Child", "triage", root);
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
  state.prByBranch[`${repo}@legion/${child}`] = `${repo}#${prNumber}`;
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

/** Every Dispatch fixture that mutates state or emits an effect on its first application, paired
 * with the state it needs to do so — one entry per fixture file under `fixtures/dispatch/`,
 * excluding `ask-opened.json` (an event type `reduceDispatchEvent` does not switch on, so it is a
 * true no-op: no mutation, no effect, no seq stamp). Used below to assert the at-most-once
 * contract holds for every one of them, not just the two hand-picked in the tests above. */
const REPLAY_ONCE_CASES: ReadonlyArray<{
  readonly name: string;
  readonly setup: () => LegionState;
  readonly event: () => DispatchIssueEvent;
}> = [
  {
    name: "issue.created root",
    setup: () => newLegionState("omp", 4),
    event: () => dispatch(issueCreatedRoot as unknown as DispatchFixture),
  },
  {
    name: "issue.created child",
    setup: () => rootState(),
    event: () => dispatch(issueCreatedChild as unknown as DispatchFixture),
  },
  {
    name: "issue.updated -> todo",
    setup: () => {
      const state = newLegionState("omp", 4);
      state.issues[root] = issueNode(root, "Root");
      return state;
    },
    event: () => dispatch(issueUpdatedTodo as unknown as DispatchFixture),
  },
  {
    name: "issue.updated human -> todo (LEGSMOKE-3)",
    setup: () => {
      const state = newLegionState("omp", 4);
      const issue = "LEGSMOKE-3" as IssueKey;
      state.issues[issue] = issueNode(issue, "T20 fixture — ask host");
      return state;
    },
    event: () => dispatch(humanTodo as unknown as DispatchFixture),
  },
  {
    name: "issue.updated -> backlog on an active tree",
    setup: () => rootState(),
    event: () => dispatch(issueUpdatedBacklog as unknown as DispatchFixture),
  },
  {
    name: "issue.updated -> icebox on an active tree",
    setup: () => rootState(),
    event: () => dispatch(issueUpdatedIcebox as unknown as DispatchFixture),
  },
  {
    name: "issue.updated -> in_progress echo",
    setup: () => {
      const state = newLegionState("omp", 4);
      state.issues[root] = issueNode(root, "Root");
      return state;
    },
    event: () => dispatch(issueUpdatedInProgress as unknown as DispatchFixture),
  },
  {
    name: "issue.updated -> testing echo",
    setup: () => {
      const state = newLegionState("omp", 4);
      state.issues[root] = issueNode(root, "Root");
      return state;
    },
    event: () => dispatch(issueUpdatedTesting as unknown as DispatchFixture),
  },
  {
    name: "issue.updated -> needs_review echo",
    setup: () => {
      const state = newLegionState("omp", 4);
      state.issues[root] = issueNode(root, "Root");
      return state;
    },
    event: () => dispatch(issueUpdatedNeedsReview as unknown as DispatchFixture),
  },
  {
    name: "issue.updated -> retro echo",
    setup: () => {
      const state = newLegionState("omp", 4);
      state.issues[root] = issueNode(root, "Root");
      return state;
    },
    event: () => dispatch(issueUpdatedRetro as unknown as DispatchFixture),
  },
  {
    name: "issue.closed root on an active tree",
    setup: () => rootState(),
    event: () => dispatch(issueClosed as unknown as DispatchFixture),
  },
  {
    name: "issue.closed child (last-child completion)",
    setup: () => {
      const state = rootState();
      attachChild(state);
      return state;
    },
    event: () => dispatchIssueWithKey(issueClosed as unknown as DispatchFixture, child),
  },
  {
    name: "child.status routed to the active parent",
    setup: () => rootState(),
    event: () => dispatch(childStatus as unknown as DispatchFixture),
  },
  {
    name: "ask.answered approves the registered design gate (legsmoke-3-ask.answered-approve.json)",
    setup: () => {
      const state = newLegionState("omp", 4);
      const issue = "LEGSMOKE-3" as IssueKey;
      state.issues[issue] = issueNode(issue, "T20 fixture — ask host");
      state.trees[issue] = { root: issue, generation: 1, status: "active", launchFailures: 0 };
      claim(state, issue, "architect");
      state.gates[issue] = { designAskId: "36e95e78-81d5-4da3-ae7b-789a16640bd9" };
      return state;
    },
    event: () => dispatch(humanApproved as unknown as DispatchFixture),
  },
  {
    name: "ask.answered approves the registered design gate (ask-answered.json)",
    setup: () => {
      const state = newLegionState("omp", 4);
      const issue = "LEGSMOKE-3" as IssueKey;
      state.issues[issue] = issueNode(issue, "T20 fixture — ask host");
      state.trees[issue] = { root: issue, generation: 1, status: "active", launchFailures: 0 };
      claim(state, issue, "architect");
      state.gates[issue] = { designAskId: "36e95e78-81d5-4da3-ae7b-789a16640bd9" };
      return state;
    },
    event: () => dispatch(askAnswered as unknown as DispatchFixture),
  },
];

describe("reduceDispatchEvent", () => {
  it("records a triage root and wakes the controller", () => {
    const state = newLegionState("omp", 4);

    expect(
      reduceDispatchEvent(state, dispatch(issueCreatedRoot as unknown as DispatchFixture), config)
    ).toEqual([{ kind: "controller", payload: { type: "triage", issue: root } }]);
    expect(state.issues[root]).toMatchObject({
      key: root,
      title: "T20 fixture capture — root",
      status: "triage",
      children: [],
    });
  });

  it("records a child and routes child-adopted to its active parent role", () => {
    const state = rootState();
    const architect = roleToken(state.project, root, "architect");

    expect(
      reduceDispatchEvent(state, dispatch(issueCreatedChild as unknown as DispatchFixture), config)
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "child-adopted", child, remaining: 1 },
      },
    ]);
    expect(state.issues[root].children).toEqual([child]);
    expect(state.issues[child]).toMatchObject({ key: child, parent: root, status: "triage" });
  });

  it("adopts the human todo transition as an admission effect", () => {
    const state = newLegionState("omp", 4);
    const issue = "LEGSMOKE-3" as IssueKey;
    state.issues[issue] = issueNode(issue, "T20 fixture — ask host");

    expect(
      reduceDispatchEvent(state, dispatch(humanTodo as unknown as DispatchFixture), config)
    ).toEqual([{ kind: "admit", issue }]);
    expect(state.issues[issue].status).toBe("todo");
  });

  it("does not re-admit an unchanged todo status", () => {
    const state = newLegionState("omp", 4);
    const issue = "LEGSMOKE-3" as IssueKey;
    state.issues[issue] = issueNode(issue, "T20 fixture — ask host", "todo");

    expect(
      reduceDispatchEvent(state, dispatch(humanTodo as unknown as DispatchFixture), config)
    ).toEqual([]);
    expect(state.issues[issue].status).toBe("todo");
  });

  for (const [status, fixture] of DAEMON_STATUS_FIXTURES) {
    it(`records the daemon-owned ${status} echo without a lifecycle effect`, () => {
      const state = newLegionState("omp", 4);
      state.issues[root] = issueNode(root, "Root");

      expect(reduceDispatchEvent(state, dispatch(fixture), config)).toEqual([]);
      expect(state.issues[root].status).toBe(status);
    });
  }

  for (const [status, fixture] of PARKED_STATUS_FIXTURES) {
    it(`records ${status} without a tree and lingers an active tree`, () => {
      const idle = newLegionState("omp", 4);
      idle.issues[root] = issueNode(root, "Root");
      expect(reduceDispatchEvent(idle, dispatch(fixture), config)).toEqual([]);
      expect(idle.issues[root].status).toBe(status);

      const active = rootState();
      expect(reduceDispatchEvent(active, dispatch(fixture), config)).toEqual([
        { kind: "linger", tree: root },
      ]);
      expect(active.issues[root].status).toBe(status);
    });
  }

  it("lingers an active root when Dispatch closes it", () => {
    const state = rootState();

    expect(
      reduceDispatchEvent(state, dispatch(issueClosed as unknown as DispatchFixture), config)
    ).toEqual([{ kind: "linger", tree: root }]);
    expect(state.issues[root].status).toBe("done");
  });

  it("routes a child close and the last-child completion edge", () => {
    const state = rootState();
    attachChild(state);
    const architect = roleToken(state.project, root, "architect");

    expect(
      reduceDispatchEvent(
        state,
        dispatchIssueWithKey(issueClosed as unknown as DispatchFixture, child),
        config
      )
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "child-closed", child, remaining: 0 },
      },
      { kind: "publish", role: architect, payload: { type: "children-complete" } },
    ]);
    expect(state.issues[child].status).toBe("done");
  });

  it("routes the Dispatch child-status payload to the active parent role", () => {
    const state = rootState();
    const architect = roleToken(state.project, root, "architect");

    expect(
      reduceDispatchEvent(state, dispatch(childStatus as unknown as DispatchFixture), config)
    ).toEqual([
      {
        kind: "publish",
        role: architect,
        payload: { type: "child-status", child, from: "triage", to: "todo" },
      },
    ]);
  });

  it("approves only the registered design ask and records its ask id", () => {
    const state = newLegionState("omp", 4);
    const issue = "LEGSMOKE-3" as IssueKey;
    state.issues[issue] = issueNode(issue, "T20 fixture — ask host");
    state.trees[issue] = {
      root: issue,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const architect = claim(state, issue, "architect");
    state.gates[issue] = { designAskId: "36e95e78-81d5-4da3-ae7b-789a16640bd9" };

    expect(
      reduceDispatchEvent(state, dispatch(humanApproved as unknown as DispatchFixture), config)
    ).toEqual([{ kind: "publish", role: architect, payload: { type: "design-approved" } }]);
    expect(state.gates[issue].designApproved).toBe("36e95e78-81d5-4da3-ae7b-789a16640bd9");

    state.gates[issue] = { designAskId: "registered-ask" };
    const unrelatedAsk = dispatch(askAnswered as unknown as DispatchFixture);
    if (
      typeof unrelatedAsk.payload !== "object" ||
      unrelatedAsk.payload === null ||
      Array.isArray(unrelatedAsk.payload)
    ) {
      throw new Error("ask.answered fixture payload must be an object");
    }
    // A higher seq than the approval's own (4), so this is fenced out by askId mismatch inside
    // reduceAskAnswered, not by the outer at-most-once seq fence — the seq fence alone would
    // also produce `[]` here (the raw fixture's own seq, 3, is lower than the approval's), which
    // would silently pass this assertion for the wrong reason.
    expect(
      reduceDispatchEvent(
        state,
        {
          ...unrelatedAsk,
          key: issue,
          seq: 100,
          payload: { ...unrelatedAsk.payload, id: "unrelated-ask" },
        },
        config
      )
    ).toEqual([]);
    expect(state.gates[issue].designApproved).toBeUndefined();
  });

  it("never re-approves or re-emits design-approved once the gate is already approved, even at a newer seq", () => {
    const state = newLegionState("omp", 4);
    const issue = "LEGSMOKE-3" as IssueKey;
    state.issues[issue] = issueNode(issue, "T20 fixture — ask host");
    state.trees[issue] = {
      root: issue,
      generation: 1,
      status: "active",
      launchFailures: 0,
    };
    const architect = claim(state, issue, "architect");
    state.gates[issue] = { designAskId: "36e95e78-81d5-4da3-ae7b-789a16640bd9" };

    expect(
      reduceDispatchEvent(state, dispatch(humanApproved as unknown as DispatchFixture), config)
    ).toEqual([{ kind: "publish", role: architect, payload: { type: "design-approved" } }]);
    expect(state.gates[issue].designApproved).toBe("36e95e78-81d5-4da3-ae7b-789a16640bd9");

    // Same ask, same answer, a strictly newer seq than the approval it already applied: the
    // outer at-most-once fence alone would let this through (100 > lastAppliedSeq), so only the
    // dedicated `gate.designApproved !== undefined` guard inside reduceAskAnswered stops the
    // re-approval and the duplicate design-approved wake.
    expect(
      reduceDispatchEvent(
        state,
        { ...dispatch(humanApproved as unknown as DispatchFixture), seq: 100 },
        config
      )
    ).toEqual([]);
    expect(state.gates[issue].designApproved).toBe("36e95e78-81d5-4da3-ae7b-789a16640bd9");
  });

  it("ignores GitHub issue webhooks without changing Dispatch lifecycle state", () => {
    const state = rootState();
    const before = structuredClone(state);

    expect(
      reduceGithubEvent(
        state,
        "notifications.github.acme.widgets.issue.1",
        github({
          action: "opened",
          issue: {
            number: 1,
            title: "GitHub issue ignored by Dispatch lifecycle",
            state: "open",
            updated_at: "2026-09-10T02:13:55.925399Z",
          },
        }),
        config
      )
    ).toEqual([]);
    expect(state).toEqual(before);
  });

  it("ignores a status update whose seq is not newer than the last one applied to the issue", () => {
    const state = newLegionState("omp", 4);
    state.issues[root] = { ...issueNode(root, "Root", "testing"), lastAppliedSeq: 999 };
    const before = structuredClone(state.issues[root]);

    expect(
      reduceDispatchEvent(state, dispatch(issueUpdatedTodo as unknown as DispatchFixture), config)
    ).toEqual([]);
    expect(state.issues[root]).toEqual(before);
  });

  it("applies a redelivered event with the same seq exactly once", () => {
    const state = newLegionState("omp", 4);
    const event = dispatch(issueCreatedRoot as unknown as DispatchFixture);

    expect(reduceDispatchEvent(state, event, config)).toEqual([
      { kind: "controller", payload: { type: "triage", issue: root } },
    ]);
    const afterFirst = structuredClone(state.issues[root]);
    expect(afterFirst.lastAppliedSeq).toBe(event.seq);

    // Exact redelivery: same event object, same seq. Must be a total no-op — no mutation
    // (including no re-derived one) and no re-emitted effect.
    expect(reduceDispatchEvent(state, event, config)).toEqual([]);
    expect(state.issues[root]).toEqual(afterFirst);
  });

  it("leaves status and children intact when an older issue.created is redelivered after a newer issue.updated", () => {
    const state = newLegionState("omp", 4);
    const createEvent = dispatch(issueCreatedRoot as unknown as DispatchFixture);
    const updateEvent = dispatch(issueUpdatedTodo as unknown as DispatchFixture);
    expect(createEvent.seq).toBeLessThan(updateEvent.seq);

    reduceDispatchEvent(state, createEvent, config);
    expect(reduceDispatchEvent(state, updateEvent, config)).toEqual([
      { kind: "admit", issue: root },
    ]);
    expect(state.issues[root].status).toBe("todo");
    const afterUpdate = structuredClone(state.issues[root]);

    // The stale create (an out-of-order redelivery) must not roll the status back to "triage",
    // touch children, or re-emit the triage wake.
    expect(reduceDispatchEvent(state, createEvent, config)).toEqual([]);
    expect(state.issues[root]).toEqual(afterUpdate);
  });

  it("never replaces an existing node on issue.created, even one whose seq clears the outer fence", () => {
    const state = newLegionState("omp", 4);
    // Constructed directly (not through a prior reduceDispatchEvent call) with a lower seq than
    // the create fixture below, so the outer at-most-once fence alone would let the create
    // through — only reduceIssueCreated's own existing-node guard must stop it from here.
    state.issues[root] = { ...issueNode(root, "Root", "todo"), lastAppliedSeq: 0 };
    state.issues[root].children.push(child);
    const before = structuredClone(state.issues[root]);

    expect(
      reduceDispatchEvent(state, dispatch(issueCreatedRoot as unknown as DispatchFixture), config)
    ).toEqual([]);
    expect(state.issues[root]).toEqual({ ...before, lastAppliedSeq: 1 });
  });

  it("ignores ask.answered against a gate with no corresponding issue node", () => {
    const state = newLegionState("omp", 4);
    const issue = "LEGSMOKE-3" as IssueKey;
    // A schema-valid but dangling gate record: registered without the issue node ever existing.
    state.gates[issue] = { designAskId: "36e95e78-81d5-4da3-ae7b-789a16640bd9" };
    const before = structuredClone(state.gates[issue]);

    expect(
      reduceDispatchEvent(state, dispatch(humanApproved as unknown as DispatchFixture), config)
    ).toEqual([]);
    expect(state.gates[issue]).toEqual(before);
    expect(state.issues[issue]).toBeUndefined();
  });

  it("ignores child.status against a tree with no corresponding issue node", () => {
    const state = newLegionState("omp", 4);
    // A schema-valid but dangling tree record: an architect is even claimed for it, but no issue
    // node was ever created — routeActive must never be reached for this key.
    state.trees[root] = { root, generation: 1, status: "active", launchFailures: 0 };
    claim(state, root, "architect");
    const before = structuredClone(state);

    expect(
      reduceDispatchEvent(state, dispatch(childStatus as unknown as DispatchFixture), config)
    ).toEqual([]);
    expect(state).toEqual(before);
  });

  for (const { name, setup, event: buildEvent } of REPLAY_ONCE_CASES) {
    it(`replays exactly once: ${name}`, () => {
      const state = setup();
      const beforeFirst = structuredClone(state);
      const event = buildEvent();

      const first = reduceDispatchEvent(state, event, config);
      const afterFirst = structuredClone(state);
      const mutated = JSON.stringify(afterFirst) !== JSON.stringify(beforeFirst);
      expect(first.length > 0 || mutated).toBe(true);

      // Exact redelivery: same event object, same seq. Must be a total no-op — no mutation
      // (including no re-derived one) and no re-emitted effect.
      expect(reduceDispatchEvent(state, event, config)).toEqual([]);
      expect(state).toEqual(afterFirst);
    });
  }
});

describe("reduceGithubEvent", () => {
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
        head_ref: childBranch,
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
    expect(state.prByBranch[`${repo}@${childBranch}`]).toBe(`${repo}#${prNumber}`);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      key: child,
      headSha: "head-sha",
      headUpdatedAt: Date.parse("2026-09-07T03:00:00Z"),
    });
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
        head_ref: childBranch,
        head_sha: "recovered-head",
      })
    ).toEqual([{ kind: "approval-status", repo, pr: prNumber, sha: "recovered-head" }]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      key: child,
      headSha: "recovered-head",
    });
    expect(state.prByBranch[`${repo}@${childBranch}`]).toBe(`${repo}#${prNumber}`);
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
        head_ref: childBranch,
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
        head_ref: childBranch,
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
        head_ref: childBranch,
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

  it("ignores a same-clock webhook synchronize that disagrees with a resync-sourced head (GitHub's authoritative read wins the tie)", () => {
    const state = rootState();
    attachChild(state);
    const T = "2026-09-07T03:00:00Z";
    // Simulates resync having already recorded head C at T (resync.ts sets
    // headUpdatedAtSource itself; here that precondition is given directly).
    addPr(state, {
      headSha: "head-c",
      headUpdatedAt: Date.parse(T),
      headUpdatedAtSource: "resync",
    });

    expect(
      effects(state, {
        kind: "pr",
        action: "synchronize",
        repo,
        number: String(prNumber),
        head_ref: childBranch,
        head_sha: "head-b",
        updated_at: T,
      })
    ).toEqual([]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      headSha: "head-c",
      headUpdatedAt: Date.parse(T),
      headUpdatedAtSource: "resync",
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
    expect(state.prByBranch[`${repo}@${childBranch}`]).toBeUndefined();
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
        head_ref: childBranch,
        head_sha: "old-sha",
        updated_at: "2026-09-07T03:00:00Z",
      })
    ).toEqual([]);
    expect(state.prs[`${repo}#${prNumber}`]).toBeUndefined();
    expect(state.prByBranch[`${repo}@${childBranch}`]).toBeUndefined();
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
        head_ref: childBranch,
        head_sha: "stale-resurrection-head",
        updated_at: "2026-09-07T03:30:00Z",
      })
    ).toEqual([]);
    expect(state.prs[`${repo}#${prNumber}`]).toBeUndefined();
  });

  it("keeps a tombstone after an unmerged close so a synchronize at the exact same clock cannot recreate the PR", () => {
    const state = rootState();
    attachChild(state);
    addPr(state);
    const closedAt = "2026-09-07T04:00:00Z";

    effects(state, {
      kind: "pr",
      action: "closed",
      repo,
      number: String(prNumber),
      merged: "false",
      updated_at: closedAt,
    });
    expect(state.prs[`${repo}#${prNumber}`]).toBeUndefined();

    // Unlike the fence between two live observations (equal clock still
    // applies — a legitimate same-second sequence), the close tombstone is
    // itself a point-in-time event: a synchronize at its exact clock is
    // the close racing its own last delivered head, not a later one.
    expect(
      effects(state, {
        kind: "pr",
        action: "synchronize",
        repo,
        number: String(prNumber),
        head_ref: childBranch,
        head_sha: "stale-resurrection-head",
        updated_at: closedAt,
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
        head_ref: childBranch,
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
        head_ref: childBranch,
        head_sha: "stale-reopen-head",
        url: "pr-url",
        updated_at: "2026-09-07T02:00:00Z",
      })
    ).toEqual([]);
    // The existing PR record must survive untouched — a stale "opened"
    // redelivery must not reset it via registerPr.
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({ headSha: "old-sha" });
  });

  it("ignores an opened redelivery at the exact same clock as the existing PR record, without wiping its CI/review state", () => {
    const state = rootState();
    attachChild(state);
    addPr(state, {
      headSha: "settled-head",
      headUpdatedAt: Date.parse("2026-09-07T03:00:00Z"),
      verdict: "green",
      ciSettledAt: 5,
      reviewDecision: "approved",
    });

    // "opened" fires exactly once per PR; a second delivery at the same
    // clock is a redelivery (the crash-after-save-before-ack window), not
    // a distinct later observation, and must not re-register the PR —
    // registerPr would wipe the verdict/review a settlement already
    // established.
    expect(
      effects(state, {
        kind: "pr",
        action: "opened",
        repo,
        number: String(prNumber),
        head_ref: childBranch,
        head_sha: "settled-head",
        url: "pr-url",
        updated_at: "2026-09-07T03:00:00Z",
      })
    ).toEqual([]);
    expect(state.prs[`${repo}#${prNumber}`]).toMatchObject({
      headSha: "settled-head",
      verdict: "green",
      ciSettledAt: 5,
      reviewDecision: "approved",
    });
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
    state.gates[child] = { designAskId: "36e95e78-81d5-4da3-ae7b-789a16640bd9" };

    expect(() =>
      reduceDispatchEvent(
        state,
        { ...dispatch(humanApproved as unknown as DispatchFixture), key: child },
        config
      )
    ).toThrow(child);
  });

  it("routes PR conversation to the tree's architect once the phase worker has already reported completion", () => {
    const state = rootState();
    attachChild(state);
    claim(state, child, "implementer");
    addPr(state);
    const phase = state.phases[child];
    if (!phase) throw new Error("phase fixture missing");
    phase.completed = { summary: "Implemented the change", at: "2026-09-09T00:00:00.000Z" };
    const architect = roleToken(state.project, root, "architect");

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
        role: architect,
        payload: {
          type: "pr-comment",
          author: "reviewer",
          body: "Please rename this",
          url: "comment-url",
        },
      },
    ]);
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

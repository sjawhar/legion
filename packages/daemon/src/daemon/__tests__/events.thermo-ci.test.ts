import { expect, it } from "bun:test";
import { formatIssueKey, roleToken, roleTopic } from "@legion/contracts";
import type { DaemonConfig } from "../config";
import { startEventPump } from "../events";
import { newLegionState } from "../legion-state";
import { reduceGithubEvent } from "../reducers";

class FakeNats {
  private readonly subscriptions: Array<{
    subject: string;
    callback: (subject: string, data: string) => void;
  }> = [];

  subscribe(subject: string, callback: (subject: string, data: string) => void): () => void {
    const subscription = { subject, callback };
    this.subscriptions.push(subscription);
    return () => {
      const index = this.subscriptions.indexOf(subscription);
      if (index >= 0) this.subscriptions.splice(index, 1);
    };
  }

  publish(): void {}

  emit(subject: string, data: string): void {
    for (const subscription of this.subscriptions) {
      if (matches(subscription.subject, subject)) subscription.callback(subject, data);
    }
  }
}

function matches(pattern: string, subject: string): boolean {
  const patternTokens = pattern.split(".");
  const subjectTokens = subject.split(".");
  for (let index = 0; index < patternTokens.length; index += 1) {
    const token = patternTokens[index];
    if (token === ">") return index < subjectTokens.length;
    if (token !== "*" && token !== subjectTokens[index]) return false;
  }
  return patternTokens.length === subjectTokens.length;
}

const config: DaemonConfig = {
  project: "omp",
  legionId: "acme/1",
  port: 13370,
  envoyUrl: "http://127.0.0.1:9020",
  natsUrls: ["nats://127.0.0.1:4222"],
  ompInvocation: "mise x github:sjawhar/oh-my-pi@18.0.3-sami.20260824-002841 -- omp",
  boardProjectIds: ["PVT_board"],
  appLogins: ["legion[bot]"],
  admissionCap: 4,
  workerBudget: 6,
  maxRecursionDepth: 8,
  lingerHours: 72,
  maxFixAttempts: 3,
  resyncIntervalMs: 600_000,
  gates: { design: "root-issues", merge: "human" },
  githubApps: {},
  stateDir: "/state",
};

function envelope(payload: Record<string, unknown>): string {
  return JSON.stringify({
    event_id: "checks-1",
    source: "github",
    source_event_id: "checks-1",
    topic: "notifications.github.acme.widgets.pr.7.checks",
    dedupe_key: "dedupe-checks-1",
    issued_at: 0,
    payload_summary: "checks settled",
    payload: JSON.stringify(payload),
    trace_id: "trace-checks-1",
  });
}

function settledChecks(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    kind: "checks",
    repo: "acme/widgets",
    number: "7",
    sha: "head-1",
    is_head: true,
    generation: 0,
    failed: { count: 0, checks: [] },
    running: { count: 0, checks: [] },
    passed: { count: 1, checks: ["unit"] },
    queued: { count: 0, checks: [] },
    skipped: { count: 0, checks: [] },
    cancelled: { count: 0, checks: [] },
    failing_checks: [],
    ...overrides,
  };
}

function stateForCi() {
  const state = newLegionState("omp", 2);
  const issue = formatIssueKey("acme", "widgets", 1);
  const architect = roleToken("omp", issue, "architect");
  const implementer = roleToken("omp", issue, "implementer");
  state.issues[issue] = {
    key: issue,
    title: "Issue one",
    state: "open",
    children: [],
    released: true,
    labels: [],
  };
  state.trees[issue] = {
    root: issue,
    generation: 1,
    status: "active",
    launchFailures: 0,
    heldEvents: [],
  };
  state.roles[architect] = { issue, role: "architect" };
  state.roles[implementer] = { issue, role: "implementer" };
  state.prs["acme/widgets#7"] = {
    key: issue,
    repo: "acme/widgets",
    number: 7,
    headSha: "head-1",
    verdict: null,
    failing: [],
    ciSettledAt: null,
    ciGeneration: null,
    fixAttempts: 0,
  };
  return { state, architect, implementer };
}

it("routes an approved PR immediately when its head's checks settle green", async () => {
  const { state, architect, implementer } = stateForCi();
  reduceGithubEvent(
    state,
    "notifications.github.acme.widgets.pr.7.review",
    {
      event_id: "review-1",
      issued_at: 0,
      payload: {
        action: "submitted",
        repository: { full_name: "acme/widgets" },
        pull_request: { number: 7, head: { sha: "head-1" } },
        review: {
          user: { login: "sami" },
          state: "approved",
          commit_id: "head-1",
          body: "lgtm",
        },
      },
    },
    config
  );

  const nats = new FakeNats();
  const published: Array<{ topic: string; payloadJson: string }> = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (topic, payloadJson) => {
      published.push({ topic, payloadJson });
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(settledChecks()));
  await pump.drain();

  expect(published).toEqual([
    {
      topic: roleTopic(implementer),
      payloadJson: JSON.stringify({ type: "ci-green", sha: "head-1" }),
    },
    {
      topic: roleTopic(architect),
      payloadJson: JSON.stringify({ type: "pr-ready", pr: 7 }),
    },
  ]);
  pump.stop();
});

it("emits settled-red when CI first settles red", async () => {
  const { state, implementer } = stateForCi();
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        failed: { count: 2, checks: ["lint", "unit"] },
        passed: { count: 0, checks: [] },
        failing_checks: [
          { name: "lint", url: "https://example.test/checks/lint" },
          { name: "unit", url: "https://example.test/checks/unit" },
        ],
      })
    )
  );
  await pump.drain();

  expect(published).toEqual([
    JSON.stringify({
      type: "ci-settled-red",
      failing: ["lint", "unit"],
      sha: "head-1",
    }),
  ]);
  expect(state.roles[implementer]).toBeDefined();
  pump.stop();
});

it("emits settled-red when a green head re-settles red", async () => {
  const { state, implementer } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    verdict: "green",
    ciSettledAt: 1,
  };
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        settled_at: 2,
        failed: { count: 1, checks: ["lint"] },
        passed: { count: 0, checks: [] },
        failing_checks: [{ name: "lint", url: "https://example.test/checks/lint" }],
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["lint"],
    ciSettledAt: 2,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["lint"], sha: "head-1" }),
  ]);
  expect(state.roles[implementer]).toBeDefined();
  pump.stop();
});
it("emits settled-red when a red head re-settles with a changed failing set", async () => {
  const { state } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    verdict: "red",
    failing: ["unit"],
    ciSettledAt: 1,
  };
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        settled_at: 2,
        failed: { count: 2, checks: ["unit", "lint"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit", "lint"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("emits settled-red when duplicate failing names change to a different multiset", async () => {
  const { state } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    verdict: "red",
    failing: ["test", "test"],
    ciSettledAt: 1,
  };
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        settled_at: 2,
        failed: { count: 2, checks: ["lint", "test"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["lint", "test"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("does not re-emit when a red head re-settles with the same failing set", async () => {
  const { state } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    verdict: "red",
    failing: ["unit"],
    ciGeneration: 0,
    ciSettledAt: 1,
  };
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        settled_at: 2,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();
  expect(state.prs["acme/widgets#7"]).toMatchObject({ ciGeneration: 0, ciSettledAt: 2 });

  expect(published).toEqual([]);
  pump.stop();
});
it("drops a lower-generation settlement for the same head", async () => {
  const { state } = stateForCi();
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ generation: 1, settled_at: 1 }))
  );
  await pump.drain();
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        generation: 0,
        settled_at: 2,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    failing: [],
    ciGeneration: 1,
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});

it("emits when a higher generation changes the failing set", async () => {
  const { state } = stateForCi();
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        generation: 0,
        settled_at: 2,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        generation: 1,
        settled_at: 1,
        failed: { count: 1, checks: ["lint"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["lint"],
    ciGeneration: 1,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
    JSON.stringify({ type: "ci-settled-red", failing: ["lint"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("ignores a settlement without a generation", async () => {
  const { state } = stateForCi();
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });
  const { generation: _generation, ...withoutGeneration } = settledChecks();

  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(withoutGeneration));
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: null,
    failing: [],
    ciGeneration: null,
  });
  expect(published).toEqual([]);
  pump.stop();
});

it("accepts a settlement for a new head at generation zero", async () => {
  const { state } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    ciGeneration: 1,
  };
  reduceGithubEvent(
    state,
    "notifications.github.acme.widgets.pull_request.synchronize",
    {
      event_id: "synchronize-1",
      issued_at: 0,
      payload: {
        kind: "pr",
        action: "synchronize",
        repo: "acme/widgets",
        number: "7",
        title: "PR title",
        author: "author",
        url: "https://github.com/acme/widgets/pull/7",
        head_sha: "head-2",
        head_ref: "legion/issue-1",
        base_ref: "main",
        merged: "false",
        merge_commit_sha: "",
        merged_by: "",
        body: "",
        updated_at: "2026-09-07T03:00:00Z",
      },
    },
    config
  );
  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config,
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ sha: "head-2", generation: 0 }))
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    headSha: "head-2",
    verdict: "green",
    ciGeneration: 0,
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-2" })]);
  pump.stop();
});

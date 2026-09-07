import { expect, it, vi } from "bun:test";
import { formatIssueKey, roleToken, roleTopic } from "@legion/contracts";
import type { CiFetchResult } from "../../state/fetch";
import type { DaemonConfig } from "../config";
import { startEventPump } from "../events";
import { type LegionState, newLegionState } from "../legion-state";
import { type Effect, reduceGithubEvent } from "../reducers";
import { runResync } from "../resync";

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
    check_runs: [{ name: "build", id: 1 }],
    generation: 0,
    snapshot: "state-hash-1",
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
    ciCheckRuns: null,
    ciSettlementGeneration: null,
    ciSnapshot: null,
    ciReconciled: false,
    fixAttempts: 0,
  };
  return { state, architect, implementer };
}

function startCiPump(state: LegionState) {
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
  return { nats, published, pump };
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
    ciCheckRuns: { build: 1 },
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
  expect(state.prs["acme/widgets#7"]).toMatchObject({ ciCheckRuns: { build: 1 }, ciSettledAt: 2 });

  expect(published).toEqual([]);
  pump.stop();
});
it("drops a lower-generation same-check-run settlement after a newer delivery", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 900 }],
        generation: 1,
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
      settledChecks({ check_runs: [{ name: "build", id: 900 }], generation: 0, settled_at: 1 })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["unit"],
    ciSettledAt: 2,
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 1,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("emits an in-order higher-generation settlement for the same check run", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({ check_runs: [{ name: "build", id: 900 }], generation: 0, settled_at: 1 })
    )
  );
  await pump.drain();
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 900 }],
        generation: 1,
        settled_at: 2,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["unit"],
    ciSettledAt: 2,
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 1,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-green", sha: "head-1" }),
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("keeps an equal check-run and generation settlement with an identical set quiet", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  const red = settledChecks({
    check_runs: [{ name: "build", id: 900 }],
    generation: 1,
    settled_at: 2,
    failed: { count: 1, checks: ["unit"] },
    passed: { count: 0, checks: [] },
  });

  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(red));
  await pump.drain();
  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(red));
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["unit"],
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 1,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("does not revive an uncertified verdict when an exact live settlement is replayed", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  const green = settledChecks({
    check_runs: [{ name: "build", id: 900 }],
    generation: 1,
    snapshot: "hash-a",
    settled_at: 1,
  });

  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(green));
  await pump.drain();
  await runResync({
    state,
    config,
    fetchGitHubProjectItems: async () => ({ items: [] }),
    fetchCiStatusBatch: async () => ({
      "acme/widgets#7": {
        ciStatus: "pending",
        mergeableStatus: null,
        headSha: "head-1",
        isOpen: true,
        updatedAt: "2026-09-07T00:00:00.000Z",
        checkRuns: { build: 900 },
      },
    }),
    applyEffects: async () => {},
    now: () => 2,
  });
  const afterResync = structuredClone(state.prs["acme/widgets#7"]);

  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(green));
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toEqual(afterResync);
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});

it("against a GitHub-authored fence an equal attempt set defers to GitHub's verdict; a newer set does not", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);

  try {
    // GitHub read first: red at {build: 900}, no listener identity, holds the tie.
    await runResync({
      state,
      config,
      fetchGitHubProjectItems: async () => ({ items: [] }),
      fetchCiStatusBatch: async () => ({
        "acme/widgets#7": {
          ciStatus: "failing",
          mergeableStatus: null,
          failingChecks: ["unit"],
          headSha: "head-1",
          isOpen: true,
          updatedAt: "2026-09-07T00:00:00.000Z",
          checkRuns: { build: 900 },
        },
      }),
      applyEffects: async () => {},
      now: () => 1,
    });
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciCheckRuns: { build: 900 },
      ciSettlementGeneration: null,
      ciReconciled: true,
    });
    const afterGithub = structuredClone(state.prs["acme/widgets#7"]);

    // Same set, disagreeing (green): GitHub holds the tie whatever the generation.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 900 }],
          generation: 5,
          snapshot: "green-hash",
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toEqual(afterGithub);

    // Same set, agreeing (red on unit): the listener identity is adopted, authority kept.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 900 }],
          generation: 5,
          snapshot: "red-hash",
          failed: { count: 1, checks: ["unit"] },
          passed: { count: 0, checks: [] },
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciCheckRuns: { build: 900 },
      ciSettlementGeneration: 5,
      ciSnapshot: "red-hash",
      ciReconciled: true,
    });
    expect(published).toEqual([]);

    // A newer set (build re-ran as 901): ordering evidence; the listener's green applies.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 901 }],
          generation: 6,
          snapshot: "rerun-hash",
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "green",
      ciCheckRuns: { build: 901 },
      ciSettlementGeneration: 6,
      ciReconciled: false,
    });
    expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  } finally {
    pump.stop();
  }
});

it("warns and drops a same-generation settlement with a different snapshot", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

  try {
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 900 }],
          generation: 1,
          snapshot: "hash-a",
        })
      )
    );
    await pump.drain();
    const beforeConflict = structuredClone(state.prs["acme/widgets#7"]);

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 900 }],
          generation: 1,
          snapshot: "hash-b",
          failed: { count: 1, checks: ["unit"] },
          passed: { count: 0, checks: [] },
        })
      )
    );
    await pump.drain();

    expect(state.prs["acme/widgets#7"]).toEqual(beforeConflict);
    expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(warn.mock.calls[0]?.join(" ")).toContain("hash-a");
    expect(warn.mock.calls[0]?.join(" ")).toContain("hash-b");
  } finally {
    warn.mockRestore();
    pump.stop();
  }
});

it("requires a nonempty snapshot and a well-formed attempt set", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ snapshot: "" }))
  );
  await pump.drain();
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ check_runs: [] }))
  );
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [
          { name: "build", id: 900 },
          { name: "build", id: 901 },
        ],
      })
    )
  );
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ check_runs: [{ name: "build", id: 0 }] }))
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: null,
    failing: [],
    ciCheckRuns: null,
  });
  expect(published).toEqual([]);
  pump.stop();
});

it("applies an equal-id live settlement after a resync fence without a generation", async () => {
  const { state } = stateForCi();

  await runResync({
    state,
    config,
    fetchGitHubProjectItems: async () => ({ items: [] }),
    fetchCiStatusBatch: async () => ({
      "acme/widgets#7": {
        ciStatus: "pending",
        mergeableStatus: null,
        headSha: "head-1",
        isOpen: true,
        updatedAt: "2026-09-07T00:00:00.000Z",
        checkRuns: { build: 900 },
      },
    }),
    applyEffects: async () => {},
    now: () => 1,
  });
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: null,
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: null,
  });

  const { nats, published, pump } = startCiPump(state);
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 900 }],
        generation: 0,
        settled_at: 2,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["unit"],
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 0,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("accepts a higher check-run id after the listener generation restarts", async () => {
  const { state } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 5,
  };
  const { nats, published, pump } = startCiPump(state);

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 901 }],
        generation: 0,
        settled_at: 2,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["unit"],
    ciCheckRuns: { build: 901 },
    ciSettlementGeneration: 0,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("ignores a settlement without a valid listener generation", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  const { generation: _generation, ...withoutGeneration } = settledChecks();

  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(withoutGeneration));
  await pump.drain();
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ generation: -1 }))
  );
  await pump.drain();
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ generation: Number.MAX_SAFE_INTEGER + 1 }))
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: null,
    failing: [],
    ciCheckRuns: null,
  });
  expect(published).toEqual([]);
  pump.stop();
});

it("does not apply a fetched green rollup after a live red settlement advances CI state", async () => {
  const { state } = stateForCi();
  const { nats, pump } = startCiPump(state);
  const fetchStarted = Promise.withResolvers<void>();
  const fetchedStatuses = Promise.withResolvers<Record<string, CiFetchResult>>();
  const resyncEffects: unknown[] = [];
  const debug = vi.spyOn(console, "debug").mockImplementation(() => {});

  try {
    const resync = runResync({
      state,
      config,
      fetchGitHubProjectItems: async () => ({ items: [] }),
      fetchCiStatusBatch: async () => {
        fetchStarted.resolve();
        return fetchedStatuses.promise;
      },
      applyEffects: async (effects) => {
        resyncEffects.push(effects);
      },
      now: () => 3,
    });
    await fetchStarted.promise;

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 900 }],
          generation: 1,
          settled_at: 2,
          failed: { count: 1, checks: ["unit"] },
          passed: { count: 0, checks: [] },
        })
      )
    );
    await pump.drain();
    const ciSettledAt = state.prs["acme/widgets#7"]?.ciSettledAt;

    fetchedStatuses.resolve({
      "acme/widgets#7": {
        ciStatus: "passing",
        mergeableStatus: null,
        headSha: "head-1",
        isOpen: true,
        updatedAt: "2026-09-07T00:00:00.000Z",
        checkRuns: { build: 850 },
      },
    });
    await resync;

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      failing: ["unit"],
      ciSettledAt,
      ciCheckRuns: { build: 900 },
      ciSettlementGeneration: 1,
    });
    expect(resyncEffects).toEqual([]);
    expect(debug).toHaveBeenCalledWith("[legion] skipped stale resync CI result acme/widgets#7");
  } finally {
    pump.stop();
    debug.mockRestore();
  }
});

it("does not uncertify a live green settlement with a stale pending rollup", async () => {
  const { state } = stateForCi();
  const { nats, pump } = startCiPump(state);
  const fetchStarted = Promise.withResolvers<void>();
  const fetchedStatuses = Promise.withResolvers<Record<string, CiFetchResult>>();
  const resyncEffects: unknown[] = [];

  const resync = runResync({
    state,
    config,
    fetchGitHubProjectItems: async () => ({ items: [] }),
    fetchCiStatusBatch: async () => {
      fetchStarted.resolve();
      return fetchedStatuses.promise;
    },
    applyEffects: async (effects) => {
      resyncEffects.push(effects);
    },
    now: () => 3,
  });
  await fetchStarted.promise;

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({ check_runs: [{ name: "build", id: 900 }], generation: 1, settled_at: 2 })
    )
  );
  await pump.drain();
  const ciSettledAt = state.prs["acme/widgets#7"]?.ciSettledAt;

  fetchedStatuses.resolve({
    "acme/widgets#7": {
      ciStatus: "pending",
      mergeableStatus: null,
      headSha: "head-1",
      isOpen: true,
      updatedAt: "2026-09-07T00:00:00.000Z",
      checkRuns: { build: 850 },
    },
  });
  await resync;

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    failing: [],
    ciSettledAt,
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 1,
  });
  expect(resyncEffects).toEqual([]);
  pump.stop();
});

it("drops a lower check-run id settlement for the same head", async () => {
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
    envelope(settledChecks({ check_runs: [{ name: "build", id: 2 }], settled_at: 1 }))
  );
  await pump.drain();
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 1 }],
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
    ciCheckRuns: { build: 2 },
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});
it("preserves a live check-run fence through a same-head status-context resync", async () => {
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
    envelope(settledChecks({ check_runs: [{ name: "build", id: 1000 }], settled_at: 1 }))
  );
  await pump.drain();

  await runResync({
    state,
    config,
    fetchGitHubProjectItems: async () => ({ items: [] }),
    fetchCiStatusBatch: async () => ({
      "acme/widgets#7": {
        ciStatus: "passing",
        mergeableStatus: null,
        headSha: "head-1",
        isOpen: true,
        updatedAt: "2026-09-07T00:00:00.000Z",
        checkRuns: {},
      },
    }),
    applyEffects: async () => {},
    now: () => 2,
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 900 }],
        settled_at: 3,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    failing: [],
    ciCheckRuns: { build: 1000 },
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});
/** One GitHub rollup for the head under test, as the fetcher would return it. */
function rollup(
  ciStatus: "passing" | "failing" | "pending",
  checkRuns: Record<string, number>,
  failingChecks: string[] = []
) {
  return {
    "acme/widgets#7": {
      ciStatus,
      failingChecks,
      cancelledCount: 0,
      mergeableStatus: null,
      headSha: "head-1",
      isOpen: true,
      updatedAt: "2026-09-07T00:00:00.000Z",
      checkRuns,
    },
  };
}

/** Runs one resync against `state` returning `status`; each call is clocked past the debounce interval. */
async function resyncWith(
  state: LegionState,
  status: ReturnType<typeof rollup>,
  applied: Effect[][] = []
): Promise<void> {
  resyncClock += config.resyncIntervalMs;
  await runResync({
    state,
    config,
    fetchGitHubProjectItems: async () => ({ items: [] }),
    fetchCiStatusBatch: async () => status,
    applyEffects: async (effects) => {
      applied.push(effects);
    },
    now: () => resyncClock,
  });
}
let resyncClock = 1;

const publishedEmissions = (applied: Effect[][]) =>
  applied.flat().flatMap((effect) => (effect.kind === "publish" ? [effect.payload] : []));

it("a pending GitHub read holds no tie: the terminal live settlement at the same set applies at once", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  try {
    // Resync sees the set while it is still running: fenced, uncertified, no authority.
    await resyncWith(state, rollup("pending", { build: 900 }));
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: null,
      ciCheckRuns: { build: 900 },
      ciSettlementGeneration: null,
      ciReconciled: false,
    });

    // The listener's terminal green at that set: pending -> terminal is forward.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({ check_runs: [{ name: "build", id: 900 }], generation: 3, settled_at: 2 })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "green",
      ciCheckRuns: { build: 900 },
      ciSettlementGeneration: 3,
      ciReconciled: false,
    });
    expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);

    // A later pending read at the same set uncertifies again without taking the tie...
    await resyncWith(state, rollup("pending", { build: 900 }));
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: null,
      ciSettlementGeneration: 3,
      ciReconciled: false,
    });
    // ...so the listener's re-versioned green (a URL-only change) re-certifies immediately.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 900 }],
          generation: 4,
          snapshot: "hash-b",
          settled_at: 3,
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "green",
      ciSettlementGeneration: 4,
      ciReconciled: false,
    });
    expect(published).toEqual([
      JSON.stringify({ type: "ci-green", sha: "head-1" }),
      JSON.stringify({ type: "ci-green", sha: "head-1" }),
    ]);
  } finally {
    pump.stop();
  }
});

it("a rollup with an older attempt set than a GitHub-authored fence is ignored; a newer one replaces it", async () => {
  const { state } = stateForCi();
  const applied: Effect[][] = [];

  // GitHub authors the fence: red at {build: 200, lint: 900}.
  await resyncWith(state, rollup("failing", { build: 200, lint: 900 }, ["build"]), applied);
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciCheckRuns: { build: 200, lint: 900 },
    ciSettlementGeneration: null,
    ciReconciled: true,
  });
  expect(publishedEmissions(applied)).toEqual([
    { type: "ci-settled-red", sha: "head-1", failing: ["build"] },
  ]);

  // An older view of the head (build still at its first attempt): ignored.
  await resyncWith(state, rollup("passing", { build: 100, lint: 900 }), applied);
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciCheckRuns: { build: 200, lint: 900 },
  });
  // A view with no check runs at all where some are fenced: ignored.
  await resyncWith(state, rollup("passing", {}), applied);
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciCheckRuns: { build: 200, lint: 900 },
  });
  // A mixed view (build newer, lint older) cannot come from one consistent read: ignored.
  await resyncWith(state, rollup("passing", { build: 300, lint: 800 }), applied);
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciCheckRuns: { build: 200, lint: 900 },
  });
  expect(publishedEmissions(applied)).toHaveLength(1);

  // GitHub's newer read (build re-ran as 300, green) replaces the fence.
  await resyncWith(state, rollup("passing", { build: 300, lint: 900 }), applied);
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    ciCheckRuns: { build: 300, lint: 900 },
    ciReconciled: true,
  });
  expect(publishedEmissions(applied).at(-1)).toEqual({ type: "ci-green", sha: "head-1" });
});

it("a rollup whose highest check run is lower than the live fence is an older view and is ignored", async () => {
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
  // The listener saw the rerun (901) succeed.
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 901 }],
        generation: 1,
        snapshot: "hash-a",
        settled_at: 1,
      })
    )
  );
  await pump.drain();
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);

  // GitHub's rollup has not indexed the rerun: its view tops out at the failed
  // run 900. Older than the fence — ignored, no red, no tie authority.
  await runResync({
    state,
    config,
    fetchGitHubProjectItems: async () => ({ items: [] }),
    fetchCiStatusBatch: async () => ({
      "acme/widgets#7": {
        ciStatus: "failing",
        failingChecks: ["build"],
        cancelledCount: 0,
        mergeableStatus: null,
        headSha: "head-1",
        isOpen: true,
        updatedAt: "2026-09-07T00:00:00.000Z",
        checkRuns: { build: 900 },
      },
    }),
    applyEffects: async () => {},
    now: () => 2,
  });
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    failing: [],
    ciCheckRuns: { build: 901 },
    ciSettlementGeneration: 1,
    ciReconciled: false,
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});

it("GitHub's authority at an attempt set survives an agreeing live refresh and clears only when the set advances", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  try {
    const set = [{ name: "build", id: 900 }];
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(settledChecks({ check_runs: set, generation: 1, settled_at: 1 }))
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({ verdict: "green", ciReconciled: false });

    // GitHub reads the same set red: it holds the tie there.
    await resyncWith(state, rollup("failing", { build: 900 }, ["build"]));
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciSettlementGeneration: 1,
      ciReconciled: true,
    });

    // Delayed green re-versioning (gen 2) at the same set disagrees: stale.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(settledChecks({ check_runs: set, generation: 2, snapshot: "hash-b", settled_at: 3 }))
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciSettlementGeneration: 1,
      ciSnapshot: "state-hash-1",
      ciReconciled: true,
    });

    // The listener's own red (gen 3) agrees: identity refreshes, authority stays.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: set,
          generation: 3,
          snapshot: "hash-c",
          settled_at: 4,
          failed: { count: 1, checks: ["build"] },
          passed: { count: 0, checks: [] },
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciSettlementGeneration: 3,
      ciSnapshot: "hash-c",
      ciReconciled: true,
    });

    // Another disagreeing green (gen 4) at the same set is still stale: agreement did not clear the protection.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(settledChecks({ check_runs: set, generation: 4, snapshot: "hash-d", settled_at: 5 }))
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciSettlementGeneration: 3,
      ciReconciled: true,
    });
    expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);

    // The set advances (build re-ran as 901): ordering evidence; green applies and authority clears.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 901 }],
          generation: 5,
          snapshot: "hash-e",
          settled_at: 6,
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "green",
      ciCheckRuns: { build: 901 },
      ciSettlementGeneration: 5,
      ciReconciled: false,
    });
    expect(published).toEqual([
      JSON.stringify({ type: "ci-green", sha: "head-1" }),
      JSON.stringify({ type: "ci-green", sha: "head-1" }),
    ]);
  } finally {
    pump.stop();
  }
});

// The controller's interleaving: from a live green fence (900, gen 1), deliver
// R (GitHub red at the same set), L2 (listener gen 2, red) and L3 (listener
// gen 3, green) in every order. GitHub's read dominates every listener event it
// did not observe, so all six end red, and no green is certified after R.
type Step = "R" | "L2" | "L3";
const orders: Step[][] = [
  ["R", "L2", "L3"],
  ["R", "L3", "L2"],
  ["L2", "R", "L3"],
  ["L2", "L3", "R"],
  ["L3", "R", "L2"],
  ["L3", "L2", "R"],
];
for (const order of orders) {
  it(`terminal verdict is order-independent: ${order.join(" -> ")} ends red`, async () => {
    const { state } = stateForCi();
    const { nats, published, pump } = startCiPump(state);
    const applied: Effect[][] = [];
    try {
      const set = [{ name: "build", id: 900 }];
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(settledChecks({ check_runs: set, generation: 1, settled_at: 1 }))
      );
      await pump.drain();
      let githubSeen = false;
      for (const step of order) {
        if (step === "R") {
          await resyncWith(state, rollup("failing", { build: 900 }, ["build"]), applied);
          githubSeen = true;
          continue;
        }
        const generation = step === "L2" ? 2 : 3;
        const red = step === "L2";
        const before = published.length;
        nats.emit(
          "notifications.github.acme.widgets.pr.7.checks",
          envelope(
            settledChecks({
              check_runs: set,
              generation,
              snapshot: `hash-${step}`,
              settled_at: generation,
              ...(red
                ? { failed: { count: 1, checks: ["build"] }, passed: { count: 0, checks: [] } }
                : {}),
            })
          )
        );
        await pump.drain();
        if (githubSeen) {
          // After GitHub has read this set, no listener event at it may certify green.
          expect(published.slice(before)).not.toContain(
            JSON.stringify({ type: "ci-green", sha: "head-1" })
          );
        }
      }
      expect(state.prs["acme/widgets#7"]).toMatchObject({
        verdict: "red",
        failing: ["build"],
        ciCheckRuns: { build: 900 },
        ciReconciled: true,
      });
    } finally {
      pump.stop();
    }
  });
}

for (const order of [
  ["L2", "L3"],
  ["L3", "L2"],
] as Step[][]) {
  it(`from a reconciled set, ${order.join(" -> ")} ends red with no green certified`, async () => {
    const { state } = stateForCi();
    const { nats, published, pump } = startCiPump(state);
    try {
      const set = [{ name: "build", id: 900 }];
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(settledChecks({ check_runs: set, generation: 1, settled_at: 1 }))
      );
      await pump.drain();
      await resyncWith(state, rollup("failing", { build: 900 }, ["build"]));
      const before = published.length;
      for (const step of order) {
        const generation = step === "L2" ? 2 : 3;
        nats.emit(
          "notifications.github.acme.widgets.pr.7.checks",
          envelope(
            settledChecks({
              check_runs: set,
              generation,
              snapshot: `hash-${step}`,
              settled_at: generation,
              ...(step === "L2"
                ? { failed: { count: 1, checks: ["build"] }, passed: { count: 0, checks: [] } }
                : {}),
            })
          )
        );
        await pump.drain();
      }
      expect(published.slice(before)).toEqual([]);
      expect(state.prs["acme/widgets#7"]).toMatchObject({ verdict: "red", ciReconciled: true });
    } finally {
      pump.stop();
    }
  });
}

it("a superseding attempt with an earlier completion is a newer set: accepted live and matched by the next read", async () => {
  // Producer trace: build#100 red and lint#900 green settle red; a delayed
  // webhook for build#200 (created later, finished earlier) replaces build#100.
  // The head's latest completion time falls while the attempt set advances.
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  const applied: Effect[][] = [];
  try {
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [
            { name: "build", id: 100 },
            { name: "lint", id: 900 },
          ],
          generation: 7,
          snapshot: "hash-red",
          settled_at: 1,
          failed: { count: 1, checks: ["build"] },
          passed: { count: 1, checks: ["lint"] },
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciCheckRuns: { build: 100, lint: 900 },
    });

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [
            { name: "build", id: 200 },
            { name: "lint", id: 900 },
          ],
          generation: 8,
          snapshot: "hash-green",
          settled_at: 2,
          superseded_settlement: "true",
          passed: { count: 2, checks: ["build", "lint"] },
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "green",
      ciCheckRuns: { build: 200, lint: 900 },
      ciSettlementGeneration: 8,
      ciReconciled: false,
    });
    expect(published).toEqual([
      JSON.stringify({ type: "ci-settled-red", failing: ["build"], sha: "head-1" }),
      JSON.stringify({ type: "ci-green", sha: "head-1" }),
    ]);

    // GitHub's next read describes the same latest set: applied quietly, holds the tie.
    await resyncWith(state, rollup("passing", { build: 200, lint: 900 }), applied);
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "green",
      ciSettlementGeneration: 8,
      ciReconciled: true,
    });
    expect(publishedEmissions(applied)).toEqual([]);
  } finally {
    pump.stop();
  }
});

it("a disagreeing live settlement at a GitHub-authored fence's set is stale: GitHub holds the tie", async () => {
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
  await runResync({
    state,
    config,
    fetchGitHubProjectItems: async () => ({ items: [] }),
    fetchCiStatusBatch: async () => ({
      "acme/widgets#7": {
        ciStatus: "failing",
        failingChecks: ["build"],
        cancelledCount: 0,
        mergeableStatus: null,
        headSha: "head-1",
        isOpen: true,
        updatedAt: "2026-09-07T00:00:00.000Z",
        checkRuns: { build: 900 },
      },
    }),
    applyEffects: async () => {},
    now: () => 2,
  });
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciSettlementGeneration: null,
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 900 }],
        generation: 0,
        snapshot: "hash-a",
        settled_at: 3,
      })
    )
  );
  await pump.drain();
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["build"],
    ciSettlementGeneration: null,
  });
  expect(published).toEqual([]);
  pump.stop();
});
it("a same-head resync refresh at an equal check-run id never erases the known generation", async () => {
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

  // Live: (900, gen 1) green settles the head.
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({ check_runs: [{ name: "build", id: 900 }], generation: 1, settled_at: 1 })
    )
  );
  await pump.drain();
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 1,
  });

  // Resync reads the same rollup (max id 900; GitHub has no generation).
  await runResync({
    state,
    config,
    fetchGitHubProjectItems: async () => ({ items: [] }),
    fetchCiStatusBatch: async () => ({
      "acme/widgets#7": {
        ciStatus: "passing",
        mergeableStatus: null,
        headSha: "head-1",
        isOpen: true,
        updatedAt: "2026-09-07T00:00:00.000Z",
        checkRuns: { build: 900 },
      },
    }),
    applyEffects: async () => {},
    now: () => 2,
  });
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 1,
  });

  // A delayed older snapshot (900, gen 0) must still be stale.
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 900 }],
        generation: 0,
        settled_at: 3,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    failing: [],
    ciCheckRuns: { build: 900 },
    ciSettlementGeneration: 1,
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});
it("drops a delayed lower check-run id and accepts a higher check-run id for the same head", async () => {
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
    envelope(settledChecks({ check_runs: [{ name: "build", id: 900 }], settled_at: 1 }))
  );
  await pump.drain();
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 850 }],
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
    ciCheckRuns: { build: 900 },
  });

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [{ name: "build", id: 950 }],
        settled_at: 3,
        failed: { count: 1, checks: ["unit"] },
        passed: { count: 0, checks: [] },
      })
    )
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    failing: ["unit"],
    ciCheckRuns: { build: 950 },
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-green", sha: "head-1" }),
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("emits when a higher check-run id changes the failing set", async () => {
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
        check_runs: [{ name: "build", id: 1 }],
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
        check_runs: [{ name: "build", id: 2 }],
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
    ciCheckRuns: { build: 2 },
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
    JSON.stringify({ type: "ci-settled-red", failing: ["lint"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("ignores a settlement without a latest check-run id", async () => {
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
  const { check_runs: _checkRuns, ...withoutCheckRuns } = settledChecks();

  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(withoutCheckRuns));
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: null,
    failing: [],
    ciCheckRuns: null,
  });
  expect(published).toEqual([]);
  pump.stop();
});

it("accepts a settlement for a new head with a lower check-run id", async () => {
  const { state } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    ciCheckRuns: { build: 2 },
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
    envelope(settledChecks({ sha: "head-2", latest_check_run_id: 1 }))
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    headSha: "head-2",
    verdict: "green",
    ciCheckRuns: { build: 1 },
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-2" })]);
  pump.stop();
});

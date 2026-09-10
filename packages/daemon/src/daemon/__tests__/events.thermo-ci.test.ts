import { expect, it, vi } from "bun:test";
import { roleTopic } from "@legion/contracts";
import type { CiFetchResult } from "../../state/fetch";
import type { CheckRunRef } from "../../state/types";
import { startEventPump } from "../events";
import type { LegionState } from "../legion-state";
import { type Effect, reduceGithubEvent } from "../reducers";
import { runResync } from "../resync";
import {
  config as daemonConfig,
  FakeNats,
  fakeDispatchClient,
  prPayload,
  settledChecks,
  stateForCi,
} from "./ci-fixtures";

const config = daemonConfig();

// Each call gets a fresh event_id, matching a real Envoy delivery (every
// notification gets a unique id): content-level duplicate/staleness
// detection (classifySettlement) is what these tests actually exercise,
// and it is unaffected by the event_id's value.
let nextEventId = 0;

function envelope(payload: Record<string, unknown>): string {
  nextEventId += 1;
  const eventId = `checks-${nextEventId}`;
  return JSON.stringify({
    event_id: eventId,
    source: "github",
    source_event_id: eventId,
    topic: "notifications.github.acme.widgets.pr.7.checks",
    dedupe_key: `dedupe-${eventId}`,
    issued_at: 0,
    payload_summary: "checks settled",
    payload: JSON.stringify(payload),
    trace_id: `trace-${eventId}`,
  });
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
    onAdmit: () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });
  return { nats, published, pump };
}

/** One GitHub rollup for the head under test, as the fetcher would return it. */
function rollup(
  ciStatus: "passing" | "failing" | "pending",
  checkRuns: CheckRunRef[],
  failures: { checks?: string[]; statuses?: string[] } = {}
) {
  return {
    "acme/widgets#7": {
      ciStatus,
      failingChecks: failures.checks ?? [],
      failingStatuses: failures.statuses ?? [],
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
    dispatchClient: fakeDispatchClient(),
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

it("routes an approved PR's settled checks and ready signal to the tree's architect", async () => {
  const { state, architect } = stateForCi();
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
    onAdmit: () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(settledChecks()));
  await pump.drain();

  expect(published).toEqual([
    {
      topic: roleTopic(architect),
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
  const { nats, published, pump } = startCiPump(state);

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
  const { nats, published, pump } = startCiPump(state);

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
    failingStatuses: [],
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
    failingStatuses: [],
    ciSettledAt: 1,
  };
  const { nats, published, pump } = startCiPump(state);

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
    failingStatuses: [],
    ciSettledAt: 1,
  };
  const { nats, published, pump } = startCiPump(state);

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
    ciCheckRuns: [{ name: "build", id: 1 }],
    ciSettledAt: 1,
  };
  const { nats, published, pump } = startCiPump(state);

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
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    ciCheckRuns: [{ name: "build", id: 1 }],
    ciSettledAt: 2,
  });

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
    failingStatuses: [],
    ciSettledAt: 2,
    ciCheckRuns: [{ name: "build", id: 900 }],
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
    failingStatuses: [],
    ciSettledAt: 2,
    ciCheckRuns: [{ name: "build", id: 900 }],
    ciSettlementGeneration: 1,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-green", sha: "head-1" }),
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
  await resyncWith(state, rollup("pending", [{ name: "build", id: 900 }]));
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
    await resyncWith(state, rollup("failing", [{ name: "build", id: 900 }], { checks: ["build"] }));
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciCheckRuns: [{ name: "build", id: 900 }],
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

    // Same set, agreeing (red on build): the listener identity is adopted, authority kept.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 900 }],
          generation: 5,
          snapshot: "red-hash",
          failed: { count: 1, checks: ["build"] },
          passed: { count: 0, checks: [] },
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciCheckRuns: [{ name: "build", id: 900 }],
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
      ciCheckRuns: [{ name: "build", id: 901 }],
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

it("applies a same-set live settlement after a pending resync fence without a generation", async () => {
  const { state } = stateForCi();

  await resyncWith(state, rollup("pending", [{ name: "build", id: 900 }]));
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: null,
    ciCheckRuns: [{ name: "build", id: 900 }],
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
    ciCheckRuns: [{ name: "build", id: 900 }],
    ciSettlementGeneration: 0,
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("accepts a newer attempt set after the listener generation restarts", async () => {
  const { state } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    ciCheckRuns: [{ name: "build", id: 900 }],
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
    ciCheckRuns: [{ name: "build", id: 901 }],
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
      dispatchClient: fakeDispatchClient(),
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
        checkRuns: [{ name: "build", id: 850 }],
      },
    });
    await resync;

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      failing: ["unit"],
      ciSettledAt,
      ciCheckRuns: [{ name: "build", id: 900 }],
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
    dispatchClient: fakeDispatchClient(),
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
      checkRuns: [{ name: "build", id: 850 }],
    },
  });
  await resync;

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    failing: [],
    ciSettledAt,
    ciCheckRuns: [{ name: "build", id: 900 }],
    ciSettlementGeneration: 1,
  });
  expect(resyncEffects).toEqual([]);
  pump.stop();
});

it("preserves a live check-run fence through a same-head status-context resync", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ check_runs: [{ name: "build", id: 1000 }], settled_at: 1 }))
  );
  await pump.drain();

  await resyncWith(state, rollup("passing", []));

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
    ciCheckRuns: [{ name: "build", id: 1000 }],
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});
it("a pending GitHub read holds no tie: the terminal live settlement at the same set applies at once", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  try {
    // Resync sees the set while it is still running: fenced, uncertified, no authority.
    await resyncWith(state, rollup("pending", [{ name: "build", id: 900 }]));
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: null,
      ciCheckRuns: [{ name: "build", id: 900 }],
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
      ciCheckRuns: [{ name: "build", id: 900 }],
      ciSettlementGeneration: 3,
      ciReconciled: false,
    });
    expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);

    // A later pending read at the same set uncertifies again without taking the tie...
    await resyncWith(state, rollup("pending", [{ name: "build", id: 900 }]));
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

it("a rollup with an older attempt set than a GitHub-authored fence is ignored; a newer one advances it", async () => {
  const { state } = stateForCi();
  const applied: Effect[][] = [];

  // GitHub authors the fence: red at {build: 200, lint: 900}.
  await resyncWith(
    state,
    rollup(
      "failing",
      [
        { name: "build", id: 200 },
        { name: "lint", id: 900 },
      ],
      { checks: ["build"] }
    ),
    applied
  );
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciCheckRuns: [
      { name: "build", id: 200 },
      { name: "lint", id: 900 },
    ],
    ciSettlementGeneration: null,
    ciReconciled: true,
  });
  expect(publishedEmissions(applied)).toEqual([
    { type: "ci-settled-red", sha: "head-1", failing: ["build"] },
  ]);

  // An older view of the head (build still at its first attempt): ignored.
  await resyncWith(
    state,
    rollup("passing", [
      { name: "build", id: 100 },
      { name: "lint", id: 900 },
    ]),
    applied
  );
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciCheckRuns: [
      { name: "build", id: 200 },
      { name: "lint", id: 900 },
    ],
  });
  // A view with no check runs at all where some are fenced: ignored.
  await resyncWith(state, rollup("passing", []), applied);
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciCheckRuns: [
      { name: "build", id: 200 },
      { name: "lint", id: 900 },
    ],
  });
  // A mixed view (build newer, lint older) cannot come from one consistent read: ignored.
  await resyncWith(
    state,
    rollup("passing", [
      { name: "build", id: 300 },
      { name: "lint", id: 800 },
    ]),
    applied
  );
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "red",
    ciCheckRuns: [
      { name: "build", id: 200 },
      { name: "lint", id: 900 },
    ],
  });
  expect(publishedEmissions(applied)).toHaveLength(1);

  // GitHub's newer read (build re-ran as 300, green) advances the fence.
  await resyncWith(
    state,
    rollup("passing", [
      { name: "build", id: 300 },
      { name: "lint", id: 900 },
    ]),
    applied
  );
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    ciCheckRuns: [
      { name: "build", id: 300 },
      { name: "lint", id: 900 },
    ],
    ciReconciled: true,
  });
  expect(publishedEmissions(applied).at(-1)).toEqual({ type: "ci-green", sha: "head-1" });
});

it("a rollup whose highest check run is lower than the live fence is an older view and is ignored", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
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
  await resyncWith(state, rollup("failing", [{ name: "build", id: 900 }], { checks: ["build"] }));
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    verdict: "green",
    failing: [],
    ciCheckRuns: [{ name: "build", id: 901 }],
    ciSettlementGeneration: 1,
    ciReconciled: false,
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});

it("GitHub's authority at an attempt set survives an agreeing live refresh and is released when the set advances", async () => {
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
    await resyncWith(state, rollup("failing", [{ name: "build", id: 900 }], { checks: ["build"] }));
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
      ciCheckRuns: [{ name: "build", id: 901 }],
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
          await resyncWith(
            state,
            rollup("failing", [{ name: "build", id: 900 }], { checks: ["build"] }),
            applied
          );
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
        // After GitHub has read this set, a listener event at it routes nothing.
        if (githubSeen) expect(published.slice(before)).toEqual([]);
      }
      expect(state.prs["acme/widgets#7"]).toMatchObject({
        verdict: "red",
        failing: ["build"],
        ciCheckRuns: [{ name: "build", id: 900 }],
        ciReconciled: true,
      });
    } finally {
      pump.stop();
    }
  });
}

for (const approved of [false, true]) {
  it(`a partial live view cannot certify a head green over a failure it omits${approved ? " (approved PR stays blocked)" : ""}`, async () => {
    const { state } = stateForCi();
    const pr = state.prs["acme/widgets#7"];
    if (!pr) throw new Error("fixture PR missing");
    if (approved) pr.reviewDecision = "approved";
    const { nats, published, pump } = startCiPump(state);
    const applied: Effect[][] = [];
    try {
      // GitHub: build passed, lint failed. Red [lint], authority held.
      await resyncWith(
        state,
        rollup(
          "failing",
          [
            { name: "build", id: 100 },
            { name: "lint", id: 900 },
          ],
          { checks: ["lint"] }
        ),
        applied
      );
      expect(pr).toMatchObject({ verdict: "red", failing: ["lint"], ciReconciled: true });

      // The listener missed lint's webhook: its record holds only build. Build reruns green.
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(
          settledChecks({
            check_runs: [{ name: "build", id: 200 }],
            generation: 4,
            snapshot: "hash-b",
            settled_at: 2,
          })
        )
      );
      await pump.drain();
      // Build advanced, lint is untouched: the fence merges, lint's failure stands, nothing is emitted.
      expect(pr).toMatchObject({
        verdict: "red",
        failing: ["lint"],
        ciCheckRuns: [
          { name: "build", id: 200 },
          { name: "lint", id: 900 },
        ],
        ciSettlementGeneration: 4,
        ciReconciled: false,
      });
      // Nothing routed: no ci-green, and for an approved PR no pr-ready.
      expect(published).toEqual([]);

      // A view that covers lint (its rerun passed) certifies green.
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(
          settledChecks({
            check_runs: [
              { name: "build", id: 200 },
              { name: "lint", id: 901 },
            ],
            generation: 5,
            snapshot: "hash-c",
            settled_at: 3,
          })
        )
      );
      await pump.drain();
      expect(pr).toMatchObject({
        verdict: "green",
        failing: [],
        ciCheckRuns: [
          { name: "build", id: 200 },
          { name: "lint", id: 901 },
        ],
      });
      expect(published).toEqual([
        JSON.stringify({ type: "ci-green", sha: "head-1" }),
        ...(approved ? [JSON.stringify({ type: "pr-ready", pr: 7 })] : []),
      ]);
    } finally {
      pump.stop();
    }
  });
}

it("a TTL-recreated sparse record at generation 0 is ordering evidence for its names only", async () => {
  const { state } = stateForCi();
  const pr = state.prs["acme/widgets#7"];
  if (!pr) throw new Error("fixture PR missing");
  const { nats, published, pump } = startCiPump(state);
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
          snapshot: "hash-a",
          settled_at: 1,
          failed: { count: 1, checks: ["lint"] },
          passed: { count: 1, checks: ["build"] },
        })
      )
    );
    await pump.drain();
    expect(published).toEqual([
      JSON.stringify({ type: "ci-settled-red", failing: ["lint"], sha: "head-1" }),
    ]);

    // The record expired; a fresh one saw only build's rerun. Generation restarts at 0.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 200 }],
          generation: 0,
          snapshot: "hash-fresh",
          settled_at: 2,
        })
      )
    );
    await pump.drain();
    expect(pr).toMatchObject({
      verdict: "red",
      failing: ["lint"],
      ciCheckRuns: [
        { name: "build", id: 200 },
        { name: "lint", id: 900 },
      ],
      ciSettlementGeneration: 0,
    });
    expect(published).toHaveLength(1);
  } finally {
    pump.stop();
  }
});

it("a name an incomplete view omitted cannot reappear as new at a lower id", async () => {
  const { state } = stateForCi();
  const pr = state.prs["acme/widgets#7"];
  if (!pr) throw new Error("fixture PR missing");
  const { nats, pump } = startCiPump(state);
  try {
    const emit = (check_runs: CheckRunRef[], generation: number) =>
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(
          settledChecks({
            check_runs,
            generation,
            snapshot: `hash-${generation}`,
            settled_at: generation,
          })
        )
      );
    emit(
      [
        { name: "a", id: 1 },
        { name: "b", id: 2 },
      ],
      1
    );
    await pump.drain();
    emit([{ name: "a", id: 2 }], 2);
    await pump.drain();
    expect(pr.ciCheckRuns).toEqual([
      { name: "a", id: 2 },
      { name: "b", id: 2 },
    ]);
    // b at 1 is older than the fence's b:2 — stale, not "new".
    emit(
      [
        { name: "a", id: 2 },
        { name: "b", id: 1 },
      ],
      3
    );
    await pump.drain();
    expect(pr).toMatchObject({
      ciCheckRuns: [
        { name: "a", id: 2 },
        { name: "b", id: 2 },
      ],
      ciSettlementGeneration: 2,
    });
  } finally {
    pump.stop();
  }
});

it("only GitHub's complete read retires a failure the listener cannot see", async () => {
  const { state } = stateForCi();
  const pr = state.prs["acme/widgets#7"];
  if (!pr) throw new Error("fixture PR missing");
  const { nats, published, pump } = startCiPump(state);
  const applied: Effect[][] = [];
  try {
    // GitHub: the check run passed but a commit status "deploy-preview" (no run id) failed.
    await resyncWith(
      state,
      rollup("failing", [{ name: "build", id: 100 }], { statuses: ["deploy-preview"] }),
      applied
    );
    expect(pr).toMatchObject({ verdict: "red", failing: [], failingStatuses: ["deploy-preview"] });
    expect(publishedEmissions(applied)).toEqual([
      { type: "ci-settled-red", sha: "head-1", failing: ["deploy-preview"] },
    ]);

    // The listener never sees statuses: its green over a newer build keeps the status failure.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "build", id: 200 }],
          generation: 3,
          snapshot: "hash-b",
          settled_at: 2,
        })
      )
    );
    await pump.drain();
    expect(pr).toMatchObject({
      verdict: "red",
      failing: [],
      failingStatuses: ["deploy-preview"],
      ciCheckRuns: [{ name: "build", id: 200 }],
    });
    expect(published).toEqual([]);

    // GitHub reads the same set with the status now passing: green.
    await resyncWith(state, rollup("passing", [{ name: "build", id: 200 }]), applied);
    expect(pr).toMatchObject({
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciReconciled: true,
    });
    expect(publishedEmissions(applied).at(-1)).toEqual({ type: "ci-green", sha: "head-1" });
  } finally {
    pump.stop();
  }
});

it("a check run that shares a failing commit status's name cannot retire the status", async () => {
  const { state } = stateForCi();
  const pr = state.prs["acme/widgets#7"];
  if (!pr) throw new Error("fixture PR missing");
  pr.reviewDecision = "approved";
  const { nats, published, pump } = startCiPump(state);
  const applied: Effect[][] = [];
  try {
    // GitHub: check run "deploy" 100 passed; commit status "deploy" failed.
    await resyncWith(
      state,
      rollup("failing", [{ name: "deploy", id: 100 }], { statuses: ["deploy"] }),
      applied
    );
    expect(pr).toMatchObject({ verdict: "red", failing: [], failingStatuses: ["deploy"] });

    // The listener reports the check run "deploy" re-running green: it observed the run, not the status.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [{ name: "deploy", id: 101 }],
          generation: 2,
          snapshot: "hash-b",
          settled_at: 2,
        })
      )
    );
    await pump.drain();
    expect(pr).toMatchObject({
      verdict: "red",
      failingStatuses: ["deploy"],
      ciCheckRuns: [{ name: "deploy", id: 101 }],
    });
    expect(published).toEqual([]);

    // GitHub reads the status passing: green, and only now pr-ready.
    await resyncWith(state, rollup("passing", [{ name: "deploy", id: 101 }]), applied);
    expect(pr).toMatchObject({ verdict: "green", failingStatuses: [] });
    expect(
      publishedEmissions(applied)
        .slice(-2)
        .map((emission) => emission.type)
    ).toEqual(["ci-green", "pr-ready"]);
  } finally {
    pump.stop();
  }
});

it("check names that collide with Object.prototype are ordinary attempt-set members", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
  const applied: Effect[][] = [];
  try {
    const first = [
      { name: "__proto__", id: 100 },
      { name: "constructor", id: 200 },
      { name: "toString", id: 300 },
    ];
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(settledChecks({ check_runs: first, generation: 1, settled_at: 1 }))
    );
    await pump.drain();
    const storedSet = () => state.prs["acme/widgets#7"]?.ciCheckRuns;
    expect(storedSet()).toEqual([
      { name: "__proto__", id: 100 },
      { name: "constructor", id: 200 },
      { name: "toString", id: 300 },
    ]);
    expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);

    // The same set from GitHub applies; a re-run of "constructor" is a newer set.
    await resyncWith(state, rollup("passing", first), applied);
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      ciSettlementGeneration: 1,
      ciReconciled: true,
    });
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [first[0], { name: "constructor", id: 201 }, first[2]],
          generation: 2,
          snapshot: "hash-b",
          settled_at: 2,
          failed: { count: 1, checks: ["constructor"] },
          passed: { count: 2, checks: ["__proto__", "toString"] },
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      ciSettlementGeneration: 2,
      ciReconciled: false,
    });
    expect(storedSet()).toEqual([
      { name: "__proto__", id: 100 },
      { name: "constructor", id: 201 },
      { name: "toString", id: 300 },
    ]);

    // A check GitHub has not shown before, named like a prototype member, is a new name: newer.
    await resyncWith(
      state,
      rollup(
        "failing",
        [
          { name: "__proto__", id: 100 },
          { name: "constructor", id: 201 },
          { name: "toString", id: 300 },
        ],
        { checks: ["constructor"] }
      )
    );
    expect(state.prs["acme/widgets#7"]).toMatchObject({ verdict: "red", ciReconciled: true });
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [
            first[0],
            { name: "constructor", id: 201 },
            { name: "hasOwnProperty", id: 400 },
            first[2],
          ],
          generation: 3,
          snapshot: "hash-c",
          settled_at: 3,
        })
      )
    );
    await pump.drain();
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "green",
      ciSettlementGeneration: 3,
      ciReconciled: false,
    });
    expect(storedSet()?.map((run) => run.name)).toEqual([
      "__proto__",
      "constructor",
      "hasOwnProperty",
      "toString",
    ]);
  } finally {
    pump.stop();
  }
});

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
      ciCheckRuns: [
        { name: "build", id: 100 },
        { name: "lint", id: 900 },
      ],
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
      ciCheckRuns: [
        { name: "build", id: 200 },
        { name: "lint", id: 900 },
      ],
      ciSettlementGeneration: 8,
      ciReconciled: false,
    });
    expect(published).toEqual([
      JSON.stringify({ type: "ci-settled-red", failing: ["build"], sha: "head-1" }),
      JSON.stringify({ type: "ci-green", sha: "head-1" }),
    ]);

    // GitHub's next read describes the same latest set: applied quietly, holds the tie.
    await resyncWith(
      state,
      rollup("passing", [
        { name: "build", id: 200 },
        { name: "lint", id: 900 },
      ]),
      applied
    );
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

it("a same-head resync read at an equal attempt set never erases the known generation", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);

  // Live: (900, gen 1) green settles the head.
  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({ check_runs: [{ name: "build", id: 900 }], generation: 1, settled_at: 1 })
    )
  );
  await pump.drain();
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    ciCheckRuns: [{ name: "build", id: 900 }],
    ciSettlementGeneration: 1,
  });

  // Resync reads the same attempt set {build: 900} (GitHub has no generation).
  await resyncWith(state, rollup("passing", [{ name: "build", id: 900 }]));
  expect(state.prs["acme/widgets#7"]).toMatchObject({
    ciCheckRuns: [{ name: "build", id: 900 }],
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
    ciCheckRuns: [{ name: "build", id: 900 }],
    ciSettlementGeneration: 1,
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
  pump.stop();
});
it("drops a delayed older attempt set and accepts a newer one for the same head", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);

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
    ciCheckRuns: [{ name: "build", id: 900 }],
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
    ciCheckRuns: [{ name: "build", id: 950 }],
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-green", sha: "head-1" }),
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("emits when a newer attempt set changes the failing set", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(
      settledChecks({
        check_runs: [
          { name: "build", id: 1 },
          { name: "unit", id: 1 },
        ],
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
        check_runs: [
          { name: "build", id: 2 },
          { name: "lint", id: 2 },
          { name: "unit", id: 2 },
        ],
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
    ciCheckRuns: [
      { name: "build", id: 2 },
      { name: "lint", id: 2 },
      { name: "unit", id: 2 },
    ],
  });
  expect(published).toEqual([
    JSON.stringify({ type: "ci-settled-red", failing: ["unit"], sha: "head-1" }),
    JSON.stringify({ type: "ci-settled-red", failing: ["lint"], sha: "head-1" }),
  ]);
  pump.stop();
});

it("ignores a settlement without an attempt set", async () => {
  const { state } = stateForCi();
  const { nats, published, pump } = startCiPump(state);
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

it("accepts a settlement for a new head with lower check-run ids", async () => {
  const { state } = stateForCi();
  state.prs["acme/widgets#7"] = {
    ...state.prs["acme/widgets#7"],
    ciCheckRuns: [{ name: "build", id: 2 }],
  };
  reduceGithubEvent(
    state,
    "notifications.github.acme.widgets.pull_request.synchronize",
    {
      event_id: "synchronize-1",
      issued_at: 0,
      payload: prPayload({ head_sha: "head-2" }),
    },
    config
  );
  const { nats, published, pump } = startCiPump(state);

  nats.emit(
    "notifications.github.acme.widgets.pr.7.checks",
    envelope(settledChecks({ sha: "head-2", check_runs: [{ name: "build", id: 1 }] }))
  );
  await pump.drain();

  expect(state.prs["acme/widgets#7"]).toMatchObject({
    headSha: "head-2",
    verdict: "green",
    ciCheckRuns: [{ name: "build", id: 1 }],
  });
  expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-2" })]);
  pump.stop();
});

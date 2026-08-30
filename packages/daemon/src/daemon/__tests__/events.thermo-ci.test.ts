import { expect, it, vi } from "bun:test";
import { formatIssueKey, roleToken, roleTopic } from "@legion/contracts";
import type { DaemonConfig } from "../config";
import { startEventPump } from "../events";
import { newLegionState } from "../legion-state";
import { reduceGithubEvent } from "../reducers";

const CI_QUIET_MS = 30_000;

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

function config(): DaemonConfig {
  return {
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
    ciQuietMs: CI_QUIET_MS,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    gates: { design: "root-issues", merge: "human" },
    githubApps: {},
    stateDir: "/state",
  };
}

function envelope(payload: Record<string, unknown>): string {
  return JSON.stringify({
    event_id: "check-1",
    source: "github",
    source_event_id: "check-1",
    topic: "notifications.github.acme.widgets.pr.7.check",
    dedupe_key: "dedupe-check-1",
    issued_at: 0,
    payload_summary: "check",
    payload: JSON.stringify(payload),
    trace_id: "trace-check-1",
  });
}

it("delays approved PR readiness until the configured CI quiet window after a green check", async () => {
  vi.useFakeTimers();

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
    checks: {},
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: false,
    lastEventAt: 0,
    fixAttempts: 0,
  };

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
    config()
  );
  expect(state.prs["acme/widgets#7"].reviewDecision).toBe("approved");

  const nats = new FakeNats();
  const published: Array<{ topic: string; payloadJson: string }> = [];
  const pump = startEventPump({
    nats,
    state,
    config: config(),
    envoyPublish: async (topic, payloadJson) => {
      published.push({ topic, payloadJson });
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  try {
    nats.emit(
      "notifications.github.acme.widgets.pr.7.check",
      envelope({
        repository: { full_name: "acme/widgets" },
        check_run: {
          head_sha: "head-1",
          name: "lint",
          status: "completed",
          conclusion: "success",
        },
      })
    );
    await pump.drain();

    expect(published).toEqual([]);

    vi.advanceTimersByTime(CI_QUIET_MS);
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
  } finally {
    pump.stop();
    vi.useRealTimers();
  }
});

it("emits settled red only after the quiet window and all observed checks complete", async () => {
  vi.useFakeTimers();

  const state = newLegionState("omp", 2);
  const issue = formatIssueKey("acme", "widgets", 1);
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
  state.roles[implementer] = { issue, role: "implementer" };
  state.prs["acme/widgets#7"] = {
    key: issue,
    repo: "acme/widgets",
    number: 7,
    headSha: "head-1",
    checks: {},
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: false,
    lastEventAt: 0,
    fixAttempts: 0,
  };

  const nats = new FakeNats();
  const published: string[] = [];
  const pump = startEventPump({
    nats,
    state,
    config: config(),
    envoyPublish: async (_topic, payloadJson) => {
      published.push(payloadJson);
    },
    saveState: async () => {},
    onException: async () => {},
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  });

  try {
    for (const checkRun of [
      { name: "lint", status: "completed", conclusion: "failure" },
      { name: "unit", status: "in_progress", conclusion: null },
    ]) {
      nats.emit(
        "notifications.github.acme.widgets.pr.7.check",
        envelope({
          repository: { full_name: "acme/widgets" },
          check_run: { head_sha: "head-1", ...checkRun },
        })
      );
      await pump.drain();
    }

    expect(published).toEqual([
      JSON.stringify({ type: "ci-first-red", check: "lint", sha: "head-1" }),
    ]);

    vi.advanceTimersByTime(CI_QUIET_MS);
    await pump.drain();

    expect(published).toEqual([
      JSON.stringify({ type: "ci-first-red", check: "lint", sha: "head-1" }),
    ]);

    nats.emit(
      "notifications.github.acme.widgets.pr.7.check",
      envelope({
        repository: { full_name: "acme/widgets" },
        check_run: {
          head_sha: "head-1",
          name: "unit",
          status: "completed",
          conclusion: "failure",
        },
      })
    );
    await pump.drain();

    vi.advanceTimersByTime(CI_QUIET_MS);
    await pump.drain();

    expect(published).toEqual([
      JSON.stringify({ type: "ci-first-red", check: "lint", sha: "head-1" }),
      JSON.stringify({
        type: "ci-settled-red",
        failing: ["lint", "unit"],
        sha: "head-1",
      }),
    ]);
  } finally {
    pump.stop();
    vi.useRealTimers();
  }
});

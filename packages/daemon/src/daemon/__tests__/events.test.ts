import { describe, expect, it, vi } from "bun:test";
import {
  controllerToken,
  formatIssueKey,
  type IssueKey,
  roleToken,
  roleTopic,
} from "@legion/contracts";
import type { DaemonConfig } from "../config";
import { type EventPumpDeps, startEventPump } from "../events";
import { type LegionState, newLegionState, type PrState } from "../legion-state";

interface Subscription {
  subject: string;
  callback: (subject: string, data: string) => void;
}

class FakeNats {
  readonly subscriptions: Subscription[] = [];

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

function envelope(payload: Record<string, unknown>, eventId = "event-1"): string {
  return JSON.stringify({
    event_id: eventId,
    source: "github",
    source_event_id: eventId,
    topic: "notifications.github.acme.widgets.issue.1.comment",
    dedupe_key: `dedupe-${eventId}`,
    issued_at: 1_000,
    payload_summary: "test",
    payload: JSON.stringify(payload),
    trace_id: `trace-${eventId}`,
  });
}

function stateForIssue(released = true): {
  state: LegionState;
  issue: IssueKey;
  architect: string;
  implementer: string;
} {
  const state = newLegionState("omp", 2);
  const issue = formatIssueKey("acme", "widgets", 1);
  const architect = roleToken("omp", issue, "architect");
  const implementer = roleToken("omp", issue, "implementer");
  state.issues[issue] = {
    key: issue,
    title: "Issue one",
    state: "open",
    children: [],
    released,
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
  return { state, issue, architect, implementer };
}

function config(): DaemonConfig {
  return {
    project: "omp",
    legionId: "acme/1",
    port: 13370,
    envoyUrl: "http://127.0.0.1:9020",
    natsUrls: ["nats://127.0.0.1:4222"],
    dispatchUrl: "http://127.0.0.1:13380",
    dispatchBearer: "dispatch-bearer",
    boardProjectIds: ["PVT_board"],
    appLogins: ["legion[bot]"],
    admissionCap: 4,
    workerBudget: 6,
    maxRecursionDepth: 8,
    lingerHours: 72,
    ciQuietMs: 5_000,
    maxFixAttempts: 3,
    resyncIntervalMs: 600_000,
    gates: { design: "root-issues", merge: "human" },
    githubApps: {},
    stateDir: "/state",
  };
}

function checkPr(issue: IssueKey, headSha = "head-1"): PrState {
  return {
    key: issue,
    repo: "acme/widgets",
    number: 7,
    headSha,
    checks: {},
    firstRedEmitted: false,
    settledRedEmitted: false,
    greenEmitted: false,
    lastEventAt: 0,
    fixAttempts: 0,
  };
}

function deps(
  state: LegionState,
  nats: FakeNats,
  envoyPublish: (topic: string, payloadJson: string) => Promise<void>,
  onException: EventPumpDeps["onException"] = async () => {},
  handlers: Pick<EventPumpDeps, "onLinger" | "onProbe" | "onApprovalStatus"> = {
    onLinger: async () => {},
    onProbe: async () => {},
    onApprovalStatus: async () => {},
  }
): EventPumpDeps {
  return {
    nats,
    envoyPublish,
    state,
    saveState: async () => {},
    onException,
    config: config(),
    ...handlers,
  };
}

async function flush(): Promise<void> {
  for (let index = 0; index < 8; index += 1) await Promise.resolve();
}

function issueComment(): Record<string, unknown> {
  return {
    action: "created",
    issue: { number: 1 },
    comment: {
      user: { login: "human" },
      body: "Please investigate",
      html_url: "https://github.com/acme/widgets/issues/1#comment",
    },
    repository: { full_name: "acme/widgets" },
  };
}

describe("core-NATS event pump", () => {
  it("reduces a GitHub envelope and publishes the resulting role event", async () => {
    const { state, architect } = stateForIssue();
    const nats = new FakeNats();
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );

    nats.emit("notifications.github.acme.widgets.issue.1.comment", envelope(issueComment()));
    await flush();

    expect(published).toEqual([
      {
        topic: roleTopic(architect),
        payloadJson: JSON.stringify({
          type: "issue-comment",
          author: "human",
          body: "Please investigate",
          url: "https://github.com/acme/widgets/issues/1#comment",
        }),
      },
    ]);
    pump.stop();
  });
  it("drains a received event after its asynchronous publication completes", async () => {
    const { state, architect } = stateForIssue();
    const nats = new FakeNats();
    const publication = Promise.withResolvers<void>();
    const published: string[] = [];
    const pump = startEventPump(
      deps(state, nats, async (topic) => {
        published.push(topic);
        await publication.promise;
      })
    );

    nats.emit("notifications.github.acme.widgets.issue.1.comment", envelope(issueComment()));
    const drained = pump.drain();
    let completed = false;
    void drained.then(() => {
      completed = true;
    });
    await flush();
    expect(completed).toBe(false);

    publication.resolve();
    await drained;
    expect(published).toEqual([roleTopic(architect)]);
    pump.stop();
  });

  it("holds role events for unreleased issues instead of publishing them", async () => {
    const { state, issue } = stateForIssue(false);
    const nats = new FakeNats();
    const published: string[] = [];
    const pump = startEventPump(
      deps(state, nats, async (topic) => {
        published.push(topic);
      })
    );

    nats.emit("notifications.github.acme.widgets.issue.1.comment", envelope(issueComment()));
    await flush();

    expect(published).toEqual([]);
    expect(state.trees[issue].heldEvents).toEqual([
      expect.objectContaining({ eventId: "event-1", role: expect.any(String) }),
    ]);
    pump.stop();
  });
  it("holds role events while a tree is queued or lingering", async () => {
    for (const status of ["queued", "lingering"] as const) {
      const { state, issue } = stateForIssue();
      state.trees[issue].status = status;
      const nats = new FakeNats();
      const published: string[] = [];
      const pump = startEventPump(
        deps(state, nats, async (topic) => {
          published.push(topic);
        })
      );

      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        envelope(issueComment(), `inactive-${status}`)
      );
      await flush();

      expect(published).toEqual([]);
      expect(state.trees[issue].heldEvents).toHaveLength(1);
      pump.stop();
    }
  });

  it("creates and eagerly routes a PR from a raw check without a branch field", async () => {
    const { state, implementer } = stateForIssue();
    state.prByBranch["acme/widgets@legion/issue-1"] = "acme/widgets#7";
    const nats = new FakeNats();
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );

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
    await flush();

    expect(state.prs["acme/widgets#7"]).toEqual(
      expect.objectContaining({
        key: formatIssueKey("acme", "widgets", 1),
        headSha: "head-1",
      })
    );
    expect(published).toEqual([
      {
        topic: roleTopic(implementer),
        payloadJson: JSON.stringify({
          type: "ci-first-red",
          check: "unit",
          sha: "head-1",
        }),
      },
    ]);
    pump.stop();
  });

  it("routes raw check observations through reduceCheck and emits eager first-red only to the implementer", async () => {
    const { state, issue, implementer } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );

    nats.emit(
      "notifications.github.acme.widgets.pr.7.check",
      envelope({
        action: "opened",
        repository: { full_name: "acme/widgets" },
        pull_request: {
          number: 7,
          head: { sha: "head-1", ref: "legion/issue-1" },
        },
        check_run: {
          head_sha: "head-1",
          name: "unit",
          status: "completed",
          conclusion: "failure",
        },
      })
    );
    await flush();

    expect(published).toEqual([
      {
        topic: roleTopic(implementer),
        payloadJson: JSON.stringify({
          type: "ci-first-red",
          check: "unit",
          sha: "head-1",
        }),
      },
    ]);
    expect(state.prs["acme/widgets#7"].checks).toEqual({
      unit: { status: "completed", conclusion: "failure" },
    });
    pump.stop();
  });

  it("publishes settled CI emissions from the five-second sweep", async () => {
    vi.useFakeTimers();
    const { state, issue, implementer } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const published: string[] = [];
    const pump = startEventPump(
      deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      })
    );

    try {
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
      await flush();
      vi.advanceTimersByTime(5_000);
      await flush();

      expect(published).toEqual([
        JSON.stringify({ type: "ci-first-red", check: "unit", sha: "head-1" }),
        JSON.stringify({
          type: "ci-settled-red",
          failing: ["unit"],
          sha: "head-1",
        }),
      ]);
      expect(state.roles[implementer]).toBeDefined();
    } finally {
      pump.stop();
      vi.useRealTimers();
    }
  });

  it("persists a failed role publication and retries it with backoff", async () => {
    vi.useFakeTimers();
    const { state, issue } = stateForIssue();
    const nats = new FakeNats();
    let attempts = 0;
    const pump = startEventPump(
      deps(state, nats, async () => {
        attempts += 1;
        if (attempts === 1) throw new Error("listener down");
      })
    );

    try {
      nats.emit("notifications.github.acme.widgets.issue.1.comment", envelope(issueComment()));
      await flush();
      expect(state.trees[issue].heldEvents).toHaveLength(1);

      vi.advanceTimersByTime(1_000);
      await flush();
      expect(attempts).toBe(2);
      expect(state.trees[issue].heldEvents).toEqual([]);
    } finally {
      pump.stop();
      vi.useRealTimers();
    }
  });

  it("passes a project issue-role delivery exception to the process manager", async () => {
    const { state, implementer } = stateForIssue();
    const nats = new FakeNats();
    const exceptions: Parameters<EventPumpDeps["onException"]>[0][] = [];
    const pump = startEventPump(
      deps(
        state,
        nats,
        async () => {},
        async (exception) => {
          exceptions.push(exception);
        }
      )
    );

    nats.emit(
      `notifications.envoy.exceptions.notifications.role.${implementer}`,
      envelope({
        original_topic: roleTopic(implementer),
        event_id: "original-event",
        reason: "no_holder",
        payload: '{"type":"pr-comment"}',
      })
    );
    await flush();

    expect(exceptions).toEqual([
      {
        roleToken: implementer,
        reason: "no_holder",
        original: {
          topic: roleTopic(implementer),
          payload: '{"type":"pr-comment"}',
          eventId: "original-event",
        },
      },
    ]);
    pump.stop();
  });

  it("marks controller delivery exceptions for the process manager", async () => {
    const { state } = stateForIssue();
    const controller = controllerToken("omp");
    const nats = new FakeNats();
    const exceptions: Parameters<EventPumpDeps["onException"]>[0][] = [];
    const pump = startEventPump(
      deps(
        state,
        nats,
        async () => {},
        async (exception) => {
          exceptions.push(exception);
        }
      )
    );

    nats.emit(
      `notifications.envoy.exceptions.notifications.role.${controller}`,
      envelope({
        original_topic: roleTopic(controller),
        event_id: "original-event",
        reason: "delivery_failed",
        payload: '{"type":"triage"}',
      })
    );
    await flush();

    expect(exceptions).toEqual([
      {
        controller: true,
        roleToken: controller,
        reason: "delivery_failed",
        original: {
          topic: roleTopic(controller),
          payload: '{"type":"triage"}',
          eventId: "original-event",
        },
      },
    ]);
    pump.stop();
  });

  it("routes lifecycle effects to the daemon dependencies", async () => {
    const { state, issue } = stateForIssue();
    const nats = new FakeNats();
    const lingered: IssueKey[] = [];
    const probed: IssueKey[] = [];
    const approvals: Array<{ kind: "approval-status"; repo: string; pr: number; sha: string }> = [];
    state.prs["acme/widgets#7"] = checkPr(issue);
    const pump = startEventPump(
      deps(
        state,
        nats,
        async () => {},
        async () => {},
        {
          onLinger: async (tree) => {
            lingered.push(tree);
          },
          onProbe: async (tree) => {
            probed.push(tree);
          },
          onApprovalStatus: async (effect) => {
            approvals.push(effect);
          },
        }
      )
    );

    nats.emit(
      "notifications.github.acme.widgets.issue.1.closed",
      envelope({
        action: "closed",
        issue: { number: 1 },
        repository: { full_name: "acme/widgets" },
      })
    );
    await flush();
    state.trees[issue].status = "closed";
    nats.emit(
      "notifications.github.acme.widgets.issue.1.reopened",
      envelope({
        action: "reopened",
        issue: { number: 1 },
        repository: { full_name: "acme/widgets" },
      })
    );
    nats.emit(
      "notifications.github.acme.widgets.pull_request.synchronize",
      envelope({
        action: "synchronize",
        repository: { full_name: "acme/widgets" },
        pull_request: { number: 7, head: { sha: "head-2", ref: "legion/issue-1" } },
      })
    );
    await flush();

    expect(lingered).toEqual([issue]);
    expect(probed).toEqual([issue]);
    expect(approvals).toEqual([
      { kind: "approval-status", repo: "acme/widgets", pr: 7, sha: "head-2" },
    ]);
    pump.stop();
  });

  it("forwards mention envelopes directly to the controller role", async () => {
    const { state } = stateForIssue();
    const nats = new FakeNats();
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );
    const payloadJson = JSON.stringify({ text: "@legion please investigate" });

    nats.emit(
      "notifications.slack.workspace.channel.mention",
      envelope({ text: "@legion please investigate" }, "mention-1")
    );
    await flush();

    expect(published).toEqual([{ topic: roleTopic(controllerToken("omp")), payloadJson }]);
    pump.stop();
  });
});

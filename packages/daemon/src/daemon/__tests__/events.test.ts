import { describe, expect, it, vi } from "bun:test";
import { controllerToken, formatIssueKey, type IssueKey, roleTopic } from "@legion/contracts";
import { overseerCatchup } from "../catchup";
import {
  type EventPumpDeps,
  startEventPump,
  truncateTermReason,
  type UndeliverableInfo,
} from "../events";
import type { LegionState } from "../legion-state";
import {
  checkPr,
  config,
  type FakeDurableControlCalls,
  FakeNats,
  prPayload,
  settledChecks,
  stateForIssue,
} from "./ci-fixtures";

function envelope(
  payload: Record<string, unknown> | string,
  eventId = "event-1",
  issuedAt = 1_000
): string {
  return JSON.stringify({
    event_id: eventId,
    source: "github",
    source_event_id: eventId,
    topic: "notifications.github.acme.widgets.issue.1.comment",
    dedupe_key: `dedupe-${eventId}`,
    issued_at: issuedAt,
    payload_summary: "test",
    payload: typeof payload === "string" ? payload : JSON.stringify(payload),
    trace_id: `trace-${eventId}`,
  });
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
    // Tests that exercise a fatal path override this with their own spy;
    // the default is a safe no-op so an unexpected fatal call never kills
    // the test runner via a real process.exit().
    fatal: async () => {},
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

  it("consumes GitHub events through a durable per-repo JetStream consumer, acking only after effects apply", async () => {
    const { state, architect } = stateForIssue();
    const nats = new FakeNats();
    const consumeDurableSpy = vi.spyOn(nats, "consumeDurable");
    const subscribeSpy = vi.spyOn(nats, "subscribe");
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );

    expect(consumeDurableSpy).toHaveBeenCalledTimes(1);
    expect(consumeDurableSpy).toHaveBeenCalledWith(
      "ENVOY_NOTIFICATIONS",
      "legion-omp-github",
      ["notifications.github.acme.widgets.>"],
      expect.any(Function)
    );
    expect(subscribeSpy).not.toHaveBeenCalledWith("notifications.github.>", expect.any(Function));

    const acks: string[] = [];
    nats.emit("notifications.github.acme.widgets.issue.1.comment", envelope(issueComment()), () =>
      acks.push("ack-1")
    );
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
    expect(acks).toEqual(["ack-1"]);
    pump.stop();
  });

  it("dispatches every effect and saves before acking — order is enforced, not incidental", async () => {
    const { state } = stateForIssue();
    const nats = new FakeNats();
    const acks: string[] = [];
    const order: string[] = [];
    const saveState = vi.fn(async () => {
      // If ack already happened, save ran too late: this assertion is the
      // regression detector, not the final state comparison alone.
      expect(acks).toEqual([]);
      order.push("save");
    });
    const pump = startEventPump({
      ...deps(state, nats, async () => {
        expect(acks).toEqual([]);
        order.push("publish");
      }),
      saveState,
    });

    try {
      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        envelope(issueComment(), "comment-1"),
        {
          ack: () => {
            acks.push("ack-1");
            order.push("ack");
          },
        }
      );
      await flush();

      expect(order).toEqual(["publish", "save", "ack"]);
      expect(acks).toEqual(["ack-1"]);
    } finally {
      pump.stop();
    }
  });

  it("dispatches every effect of a multi-effect reducer input before saving; the publish hook never sees an ack", async () => {
    const { state, issue, implementer } = stateForIssue();
    state.phases[issue] = { phase: "implementer", sessionId: "worker-session" };
    state.prs["acme/widgets#7"] = checkPr(issue, { verdict: "green" });
    const nats = new FakeNats();
    const acks: string[] = [];
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const approvalStatusCalls: unknown[] = [];
    const saveState = vi.fn(async () => {
      // A review approving the PR's current, green head derives three
      // effects (pr-review publish, approval-status, pr-ready publish);
      // save must not run until every one of them has dispatched.
      expect(published).toHaveLength(2);
      expect(approvalStatusCalls).toHaveLength(1);
      expect(acks).toEqual([]);
    });
    const pump = startEventPump({
      ...deps(
        state,
        nats,
        async (topic, payloadJson) => {
          expect(acks).toEqual([]);
          published.push({ topic, payloadJson });
        },
        undefined,
        {
          onLinger: async () => {},
          onProbe: async () => {},
          onApprovalStatus: async (effect) => {
            expect(acks).toEqual([]);
            approvalStatusCalls.push(effect);
          },
        }
      ),
      saveState,
    });

    try {
      nats.emit(
        "notifications.github.acme.widgets.pull_request_review.submitted",
        envelope(
          {
            action: "submitted",
            repository: { full_name: "acme/widgets" },
            pull_request: { number: 7, head: { sha: "head-1" } },
            review: {
              user: { login: "sami" },
              state: "approved",
              commit_id: "head-1",
              body: "Looks good",
            },
          },
          "review-1"
        ),
        { ack: () => acks.push("ack-1") }
      );
      await pump.drain();

      expect(published).toEqual([
        {
          topic: roleTopic(implementer),
          payloadJson: JSON.stringify({
            type: "pr-review",
            state: "approved",
            author: "sami",
            body: "Looks good",
          }),
        },
        {
          topic: roleTopic(implementer),
          payloadJson: JSON.stringify({ type: "pr-ready", pr: 7 }),
        },
      ]);
      expect(approvalStatusCalls).toEqual([
        { kind: "approval-status", repo: "acme/widgets", pr: 7, sha: "head-1" },
      ]);
      expect(saveState).toHaveBeenCalledTimes(1);
      expect(acks).toEqual(["ack-1"]);
    } finally {
      pump.stop();
    }
  });

  it("acks after saving; a 404 no-holder publish calls onUndeliverable with subject, event id, and effect kind instead of failing", async () => {
    const { state, architect } = stateForIssue();
    const nats = new FakeNats();
    const undeliverable: UndeliverableInfo[] = [];
    const pump = startEventPump({
      ...deps(state, nats, async () => {
        const error = new Error("Envoy publish failed with status 404") as Error & {
          status?: number;
        };
        error.status = 404;
        throw error;
      }),
      onUndeliverable: async (info) => {
        undeliverable.push(info);
      },
    });

    try {
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        envelope(issueComment(), "comment-1"),
        {},
        calls
      );
      await flush();

      expect(calls).toEqual({ acks: 1, naks: [], terms: [] });
      expect(undeliverable).toHaveLength(1);
      expect(undeliverable[0]).toMatchObject({
        role: architect,
        eventId: "comment-1",
        subject: "notifications.github.acme.widgets.issue.1.comment",
        summary: "test",
        kind: "publish",
      });
    } finally {
      pump.stop();
    }
  });

  it("logs the subject and payload_summary in the default onUndeliverable message when no hook is provided", async () => {
    const { state, architect } = stateForIssue();
    const nats = new FakeNats();
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump(
      deps(state, nats, async () => {
        const error = new Error("Envoy publish failed with status 404") as Error & {
          status?: number;
        };
        error.status = 404;
        throw error;
      })
    );

    try {
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        envelope(issueComment(), "comment-1"),
        {},
        calls
      );
      await flush();

      expect(calls).toEqual({ acks: 1, naks: [], terms: [] });
      expect(errorLog).toHaveBeenCalledWith(
        expect.stringContaining("notifications.github.acme.widgets.issue.1.comment")
      );
      expect(errorLog).toHaveBeenCalledWith(expect.stringContaining("summary=test"));
      expect(errorLog).toHaveBeenCalledWith(expect.stringContaining(`no holder for ${architect}`));
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });

  it("calls fatal (never ack or nak) when a non-404 publish fails during effect dispatch", async () => {
    const { state } = stateForIssue();
    const nats = new FakeNats();
    const fatalCalls: unknown[] = [];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump({
      ...deps(state, nats, async () => {
        throw new Error("listener down");
      }),
      fatal: async (error) => {
        fatalCalls.push(error);
      },
    });

    try {
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        envelope(issueComment(), "comment-1"),
        {},
        calls
      );
      await flush();

      // The reducer's mutation is already in memory when the publish
      // fails; going fatal (not ack, not nak) means a supervisor restart
      // reloads clean state from disk instead of continuing to serve
      // other messages against it.
      expect(fatalCalls).toHaveLength(1);
      expect(calls).toEqual({ acks: 0, naks: [], terms: [] });
      expect(errorLog).toHaveBeenCalledWith(
        expect.stringContaining(
          "durable message fatally failed on notifications.github.acme.widgets.issue.1.comment"
        )
      );
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });

  it("terms then calls fatal when a reducer mutates state and then throws", async () => {
    const { state, issue } = stateForIssue();
    state.phases[issue] = { phase: "not-a-real-role", sessionId: "corrupt-session" };
    const nats = new FakeNats();
    const fatalCalls: unknown[] = [];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump({
      ...deps(state, nats, async () => {}),
      fatal: async (error) => {
        fatalCalls.push(error);
      },
    });

    try {
      const childKey = formatIssueKey("acme", "widgets", 2);
      const subIssuePayload = {
        action: "sub_issue_added",
        repository: { full_name: "acme/widgets" },
        parent_issue: { number: 1, updated_at: "2026-01-01T00:00:00.000Z" },
        sub_issue: { number: 2, title: "Child", state: "open", labels: [] },
      };
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      nats.emit(
        "notifications.github.acme.widgets.issue.1.sub_issue",
        envelope(subIssuePayload, "corrupt-phase-1"),
        { streamSequence: 3, deliverySequence: 1 },
        calls
      );
      await flush();

      // subIssue() pushes the child into parent.children before routeActive
      // throws on the corrupted phase; there is no dispatch, no save, and
      // no restore — the mutation stays exactly where the reducer left it.
      // Reducers are synchronous and pure, so this throw is deterministic:
      // the message is termed with a loud poison log (never redelivered),
      // and the process then goes fatal since memory may be dirty.
      expect(state.issues[issue].children).toEqual([childKey]);
      expect(calls).toEqual({ acks: 0, naks: [], terms: [expect.any(String)] });
      expect(fatalCalls).toHaveLength(1);
      // The term frame is only in the client's outgoing buffer until
      // flushed; a flush before the fatal exit is what keeps JetStream
      // from redelivering a message the daemon already decided to term.
      expect(nats.flushCalls).toBe(1);
      const [message] = errorLog.mock.calls[0] ?? [];
      expect(message).toContain("notifications.github.acme.widgets.issue.1.sub_issue");
      expect(message).toContain("stream_seq=3");
      expect(message).toContain("event_id=corrupt-phase-1");
      expect(message).toContain("unrecognized phase");
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });

  it("calls fatal even when terming or flushing a poison message itself throws", async () => {
    const { state, issue } = stateForIssue();
    state.phases[issue] = { phase: "not-a-real-role", sessionId: "corrupt-session" };
    const nats = new FakeNats();
    nats.flush = () => {
      throw new Error("connection draining");
    };
    const fatalCalls: unknown[] = [];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump({
      ...deps(state, nats, async () => {}),
      fatal: async (error) => {
        fatalCalls.push(error);
      },
    });

    try {
      const subIssuePayload = {
        action: "sub_issue_added",
        repository: { full_name: "acme/widgets" },
        parent_issue: { number: 1, updated_at: "2026-01-01T00:00:00.000Z" },
        sub_issue: { number: 2, title: "Child", state: "open", labels: [] },
      };
      nats.emit(
        "notifications.github.acme.widgets.issue.1.sub_issue",
        envelope(subIssuePayload, "corrupt-phase-2")
      );
      await flush();

      // A rejected flush (or a term call that throws outright) must never
      // suppress the fatal exit: the reducer has already mutated live
      // state, so the process must not keep serving other messages against
      // it regardless of whether the term frame made it out.
      expect(fatalCalls).toHaveLength(1);
      expect(
        errorLog.mock.calls.some(([message]) => String(message).includes("connection draining"))
      ).toBe(true);
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });

  it("warns and ignores a raw-shaped GitHub pull request payload", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const pump = startEventPump(deps(state, nats, async () => {}));

    try {
      nats.emit(
        "notifications.github.acme.widgets.pull_request.synchronize",
        envelope({
          action: "synchronize",
          repository: { full_name: "acme/widgets" },
          pull_request: { number: 7, head: { sha: "unexpected-head" } },
        })
      );
      await pump.drain();

      expect(warn).toHaveBeenCalledTimes(1);
      expect(warn).toHaveBeenCalledWith(
        "legion: ignored raw-shaped GitHub payload (nested pull_request); Envoy emits kind/action/head_sha"
      );
      expect(state.prs["acme/widgets#7"]).toEqual(checkPr(issue));
    } finally {
      pump.stop();
      warn.mockRestore();
    }
  });

  it("routes raw review and review-comment payloads with a nested pull request", async () => {
    const { state, issue, implementer } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    state.phases[issue] = { phase: "implementer", sessionId: "worker-session" };
    const nats = new FakeNats();
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );

    try {
      nats.emit(
        "notifications.github.acme.widgets.pull_request_review.submitted",
        envelope(
          {
            action: "submitted",
            repository: { full_name: "acme/widgets" },
            pull_request: { number: 7, head: { sha: "head-1" } },
            review: {
              user: { login: "reviewer" },
              state: "approved",
              commit_id: "head-1",
              body: "Looks good",
            },
          },
          "review-submitted"
        )
      );
      await pump.drain();
      nats.emit(
        "notifications.github.acme.widgets.pull_request_review_comment.created",
        envelope(
          {
            action: "created",
            repository: { full_name: "acme/widgets" },
            pull_request: { number: 7, head: { sha: "head-1" } },
            comment: {
              user: { login: "reviewer" },
              body: "Please rename this",
              path: "src/index.ts",
              html_url: "https://github.com/acme/widgets/pull/7#discussion_r1",
            },
          },
          "review-comment-created"
        )
      );
      await pump.drain();

      expect(warn).not.toHaveBeenCalled();
      expect(state.prs["acme/widgets#7"]?.reviewDecision).toBe("approved");
      expect(published).toEqual([
        {
          topic: roleTopic(implementer),
          payloadJson: JSON.stringify({
            type: "pr-review",
            state: "approved",
            author: "reviewer",
            body: "Looks good",
          }),
        },
        {
          topic: roleTopic(implementer),
          payloadJson: JSON.stringify({
            type: "pr-review-comment",
            author: "reviewer",
            body: "Please rename this",
            path: "src/index.ts",
            url: "https://github.com/acme/widgets/pull/7#discussion_r1",
          }),
        },
      ]);
    } finally {
      pump.stop();
      warn.mockRestore();
    }
  });

  it("logs the subject and event ID after consuming a GitHub envelope", async () => {
    const { state, architect } = stateForIssue();
    const nats = new FakeNats();
    const log = vi.spyOn(console, "log").mockImplementation(() => {});
    const pump = startEventPump(deps(state, nats, async () => {}));

    nats.emit("notifications.github.acme.widgets.issue.1.comment", envelope(issueComment()));
    await flush();

    expect(log).toHaveBeenCalledWith(
      `[legion] consumed event event-1 subject=notifications.github.acme.widgets.issue.1.comment`
    );
    expect(state.roles[architect]).toBeDefined();
    pump.stop();
    log.mockRestore();
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

  it("publishes role events immediately for unreleased issues (release no longer gates delivery)", async () => {
    const { state, architect } = stateForIssue(false);
    const nats = new FakeNats();
    const published: string[] = [];
    const pump = startEventPump(
      deps(state, nats, async (topic) => {
        published.push(topic);
      })
    );

    nats.emit("notifications.github.acme.widgets.issue.1.comment", envelope(issueComment()));
    await flush();

    expect(published).toEqual([roleTopic(architect)]);
    pump.stop();
  });
  it("publishes role events immediately even while a tree is queued or lingering", async () => {
    for (const status of ["queued", "lingering"] as const) {
      const { state, issue, architect } = stateForIssue();
      state.trees[issue].status = status;
      const nats = new FakeNats();
      const published: string[] = [];
      const acks: string[] = [];
      const pump = startEventPump(
        deps(state, nats, async (topic) => {
          published.push(topic);
        })
      );

      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        envelope(issueComment(), `${status}-comment`),
        () => acks.push(status)
      );
      await flush();

      expect(published).toEqual([roleTopic(architect)]);
      expect(acks).toEqual([status]);
      expect(state.trees[issue].heldEvents).toEqual([]);
      pump.stop();
    }
  });

  it("does not derive a PR head from checks without a synchronize event", async () => {
    const { state } = stateForIssue();
    state.prByBranch["acme/widgets@legion/issue-1"] = "acme/widgets#7";
    const nats = new FakeNats();
    const published: string[] = [];
    const pump = startEventPump(
      deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      })
    );

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          failed: { count: 1, checks: ["unit"] },
          passed: { count: 0, checks: [] },
        })
      )
    );
    await pump.drain();

    expect(state.prs["acme/widgets#7"]).toBeUndefined();
    expect(published).toEqual([]);
    pump.stop();
  });

  it("persists only accepted checks settlements, including ones without a CI emission", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const published: string[] = [];
    const saveState = vi.fn(async () => {});
    const pump = startEventPump({
      ...deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      }),
      saveState,
    });

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(settledChecks({ sha: "stale-head" }), "ignored")
    );
    await pump.drain();
    expect(saveState).not.toHaveBeenCalled();

    nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(settledChecks()));
    await pump.drain();
    expect(saveState).toHaveBeenCalled();
    const savesAfterEmission = saveState.mock.calls.length;
    expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({ check_runs: [{ name: "build", id: 2 }], settled_at: 2_000 }),
        "settled-without-emission"
      )
    );
    await pump.drain();
    // One save per event, regardless of how many effects it derives: the
    // reducer runs, its effects dispatch, then state saves once (see
    // applyDurableEvent) — a zero-effect settlement is no different from
    // a one-effect settlement here.
    expect(saveState).toHaveBeenCalledTimes(savesAfterEmission + 1);
    expect(state.prs["acme/widgets#7"]).toMatchObject({
      ciSettledAt: 2_000,
      ciCheckRuns: [{ name: "build", id: 2 }],
    });
    expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
    pump.stop();
  });

  it("calls fatal (never ack or nak) when saveState rejects after the reducer's effects already dispatched", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const published: string[] = [];
    const fatalCalls: unknown[] = [];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const saveState = vi.fn(async () => {
      throw new Error("disk full");
    });
    const pump = startEventPump({
      ...deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      }),
      saveState,
      fatal: async (error) => {
        fatalCalls.push(error);
      },
    });

    try {
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(settledChecks(), "settle-attempt-1"),
        {},
        calls
      );
      await flush();

      // The publish already ran (it dispatches before save); only the
      // save afterward failed. Still fatal, not nak: the mutation and the
      // fact its effect already published are only in memory until the
      // save durably records them, so the process must not keep serving
      // other messages against this now-unconfirmed state.
      expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
      expect(calls).toEqual({ acks: 0, naks: [], terms: [] });
      expect(fatalCalls).toHaveLength(1);
      expect(errorLog).toHaveBeenCalledWith(
        expect.stringContaining(
          "durable message fatally failed on notifications.github.acme.widgets.pr.7.checks"
        )
      );
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });
  it("ignores checks for a SHA that is not the current head", async () => {
    const { state, issue } = stateForIssue();
    // A settled green head: the non-head settlement must leave it untouched.
    const before = checkPr(issue, {
      headSha: "current-head",
      headUpdatedAt: 2_000,
      verdict: "green",
      ciSettledAt: 1_500,
    });
    state.prs["acme/widgets#7"] = structuredClone(before);
    const nats = new FakeNats();
    const published: string[] = [];
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    const pump = startEventPump(
      deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      })
    );

    try {
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(settledChecks({ sha: "settled-head" }))
      );
      await pump.drain();

      expect(state.prs["acme/widgets#7"]).toEqual(before);
      expect(published).toEqual([]);
      expect(debug).toHaveBeenCalledWith(
        "[legion] ignored non-head checks event event-1 subject=notifications.github.acme.widgets.pr.7.checks sha=settled-head head_sha=current-head"
      );
    } finally {
      pump.stop();
      debug.mockRestore();
    }
  });
  it("keeps a higher check-run id green settlement when a lower id arrives for the same SHA", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const published: string[] = [];
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    const pump = startEventPump(
      deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      })
    );

    try {
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(
          settledChecks({ check_runs: [{ name: "build", id: 2 }], settled_at: 2_000 }),
          "newer-green"
        )
      );
      await pump.drain();
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(
          settledChecks({
            check_runs: [{ name: "build", id: 1 }],
            settled_at: 1_000,
            failed: { count: 1, checks: ["unit"] },
            passed: { count: 0, checks: [] },
          }),
          "older-red"
        )
      );
      await pump.drain();

      expect(state.prs["acme/widgets#7"]).toMatchObject({
        headSha: "head-1",
        verdict: "green",
        failing: [],
        failingStatuses: [],
        ciSettledAt: 2_000,
        ciCheckRuns: [{ name: "build", id: 2 }],
      });
      expect(published).toEqual([JSON.stringify({ type: "ci-green", sha: "head-1" })]);
      expect(debug).toHaveBeenCalledWith(
        "[legion] ignored stale checks event older-red subject=notifications.github.acme.widgets.pr.7.checks sha=head-1"
      );
    } finally {
      pump.stop();
      debug.mockRestore();
    }
  });

  it("accepts a newer synchronize despite an earlier checks wall-clock settlement", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = {
      ...checkPr(issue),
      headUpdatedAt: 1_000,
    };
    const nats = new FakeNats();
    const published: string[] = [];
    const pump = startEventPump(
      deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      })
    );

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          sha: "head-2",
          settled_at: 10_000,
          failed: { count: 1, checks: ["unit"] },
          passed: { count: 0, checks: [] },
        }),
        "head-2-settled"
      )
    );
    await pump.drain();
    nats.emit(
      "notifications.github.acme.widgets.pull_request.synchronize",
      envelope(
        prPayload({
          head_sha: "head-3",
          updated_at: "1970-01-01T00:00:02.000Z",
        })
      )
    );
    await pump.drain();

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      headSha: "head-3",
      headUpdatedAt: 2_000,
      verdict: null,
      failing: [],
      failingStatuses: [],
      ciSettledAt: null,
    });
    expect(published).toEqual([]);
    pump.stop();
  });
  it("clears a re-settled head's red flags before tracking the next head", async () => {
    const { state, issue, implementer } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    state.phases[issue] = { phase: "implementer", sessionId: "worker-session" };
    const nats = new FakeNats();
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [
            { name: "build", id: 1 },
            { name: "unit", id: 2 },
          ],
          failed: { count: 1, checks: ["unit"] },
          passed: { count: 1, checks: ["build"] },
          failing_checks: [{ name: "unit", url: "https://example.test/checks/unit" }],
        }),
        "checks-red"
      )
    );
    await flush();
    // unit reruns green: the re-settlement reports it, so its failure clears.
    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          check_runs: [
            { name: "build", id: 1 },
            { name: "unit", id: 3 },
          ],
          passed: { count: 2, checks: ["build", "unit"] },
          generation: 1,
          snapshot: "state-hash-2",
          settled_at: 2_000,
          superseded_settlement: "true",
        }),
        "checks-green"
      )
    );
    await flush();

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      headSha: "head-1",
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciSettledAt: 2_000,
      fixAttempts: 0,
    });
    expect(await overseerCatchup(state, issue)).toMatchObject({
      prVerdicts: {
        "acme/widgets#7": { sha: "head-1", ci: "green", fixAttempts: 0 },
      },
    });

    nats.emit(
      "notifications.github.acme.widgets.pull_request.synchronize",
      envelope(
        prPayload({
          head_sha: "head-2",
          updated_at: "2026-09-07T03:02:00Z",
        }),
        "pr-synchronize"
      )
    );
    await flush();

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      headSha: "head-2",
      verdict: null,
      failing: [],
      failingStatuses: [],
      ciSettledAt: null,
      fixAttempts: 0,
    });
    expect(published).toEqual([
      {
        topic: roleTopic(implementer),
        payloadJson: JSON.stringify({
          type: "ci-settled-red",
          failing: ["unit"],
          sha: "head-1",
        }),
      },
      {
        topic: roleTopic(implementer),
        payloadJson: JSON.stringify({ type: "ci-green", sha: "head-1" }),
      },
    ]);
    pump.stop();
  });

  it("uncertifies a green CI verdict for a cancelled-only settled envelope", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = {
      ...checkPr(issue),
      verdict: "green",
      ciSettledAt: 500,
    };
    const nats = new FakeNats();
    const published: string[] = [];
    const pump = startEventPump(
      deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      })
    );

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          passed: { count: 0, checks: [] },
          cancelled: { count: 1, checks: ["cancelled-job"] },
        })
      )
    );
    await flush();

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: null,
      failing: [],
      failingStatuses: [],
      ciSettledAt: 1_000,
    });
    expect(published).toEqual([]);
    pump.stop();
  });

  it("does not change a red CI verdict for a cancelled-only settled envelope", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = {
      ...checkPr(issue),
      verdict: "red",
      failing: ["unit"],
      failingStatuses: [],
      ciSettledAt: 500,
    };
    const nats = new FakeNats();
    const published: string[] = [];
    const pump = startEventPump(
      deps(state, nats, async (_topic, payloadJson) => {
        published.push(payloadJson);
      })
    );

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          passed: { count: 0, checks: [] },
          cancelled: { count: 1, checks: ["cancelled-job"] },
        })
      )
    );
    await flush();

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "red",
      failing: ["unit"],
      failingStatuses: [],
      ciSettledAt: 1_000,
    });
    expect(published).toEqual([]);
    pump.stop();
  });

  it("accepts checks payloads without the retired is_head field", async () => {
    const { state, issue, implementer } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    state.phases[issue] = { phase: "implementer", sessionId: "worker-session" };
    const nats = new FakeNats();
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );
    const payload = settledChecks();
    delete payload.is_head;

    nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope(payload));
    await pump.drain();

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      verdict: "green",
      failing: [],
      failingStatuses: [],
      ciSettledAt: 1_000,
    });
    expect(published).toEqual([
      {
        topic: roleTopic(implementer),
        payloadJson: JSON.stringify({ type: "ci-green", sha: "head-1" }),
      },
    ]);
    pump.stop();
  });

  it("ignores an explicit non-head checks payload without failing the pump", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    const pump = startEventPump(deps(state, nats, async () => {}));

    try {
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(settledChecks({ is_head: false }))
      );
      await expect(pump.drain()).resolves.toBeUndefined();

      expect(state.prs["acme/widgets#7"]).toEqual(checkPr(issue));
      expect(debug).toHaveBeenCalledWith(
        "[legion] ignored malformed or non-head checks event event-1 subject=notifications.github.acme.widgets.pr.7.checks"
      );
    } finally {
      pump.stop();
      debug.mockRestore();
    }
  });

  it("ignores malformed checks payload JSON without failing the pump", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const debug = vi.spyOn(console, "debug").mockImplementation(() => {});
    const pump = startEventPump(deps(state, nats, async () => {}));

    try {
      nats.emit("notifications.github.acme.widgets.pr.7.checks", envelope("{", "malformed-check"));
      await expect(pump.drain()).resolves.toBeUndefined();

      expect(state.prs["acme/widgets#7"]).toEqual(checkPr(issue));
      expect(debug).toHaveBeenCalledWith(
        "[legion] ignored malformed or non-head checks event malformed-check subject=notifications.github.acme.widgets.pr.7.checks"
      );
    } finally {
      pump.stop();
      debug.mockRestore();
    }
  });

  it("uses payload settled_at before envelope issued_at for the current head", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = {
      ...checkPr(issue),
      verdict: "green",
      ciSettledAt: 1_500,
    };
    const nats = new FakeNats();
    const pump = startEventPump(deps(state, nats, async () => {}));

    nats.emit(
      "notifications.github.acme.widgets.pr.7.checks",
      envelope(
        settledChecks({
          settled_at: 2_000,
          failed: { count: 1, checks: ["unit"] },
          passed: { count: 0, checks: [] },
        }),
        "newer-settlement",
        1_000
      )
    );
    await pump.drain();

    expect(state.prs["acme/widgets#7"]).toMatchObject({
      headSha: "head-1",
      verdict: "red",
      failing: ["unit"],
      failingStatuses: [],
      ciSettledAt: 2_000,
    });
    pump.stop();
  });
  it("calls fatal and adds nothing to heldEvents when a durable GitHub event's publish effect rejects (non-404)", async () => {
    const { state, issue } = stateForIssue();
    const nats = new FakeNats();
    const fatalCalls: unknown[] = [];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump({
      ...deps(state, nats, async () => {
        throw new Error("listener down");
      }),
      fatal: async (error) => {
        fatalCalls.push(error);
      },
    });

    try {
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        envelope(issueComment()),
        {},
        calls
      );
      await flush();

      expect(calls).toEqual({ acks: 0, naks: [], terms: [] });
      expect(fatalCalls).toHaveLength(1);
      // The durable lane never holds effects out-of-band (that mechanism
      // is for the mention/exception/resync held lane only): a lost
      // publish here goes fatal, never into heldEvents.
      expect(state.trees[issue].heldEvents).toEqual([]);
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });
  it("calls fatal and adds nothing to controllerHeldEvents when a durable GitHub event's controller effect rejects (non-404)", async () => {
    const { state, issue } = stateForIssue();
    state.trees[issue].status = "closed";
    const nats = new FakeNats();
    const fatalCalls: unknown[] = [];
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump({
      ...deps(state, nats, async () => {
        throw new Error("controller listener is unavailable");
      }),
      fatal: async (error) => {
        fatalCalls.push(error);
      },
    });

    try {
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      nats.emit(
        "notifications.github.acme.widgets.issue.1",
        envelope({
          action: "reopened",
          repository: { full_name: "acme/widgets" },
          issue: { number: 1, state: "open", updated_at: "2026-01-01T00:00:00.000Z" },
        }),
        {},
        calls
      );
      await flush();

      expect(calls).toEqual({ acks: 0, naks: [], terms: [] });
      expect(fatalCalls).toHaveLength(1);
      expect(state.controllerHeldEvents).toEqual([]);
    } finally {
      errorLog.mockRestore();
      pump.stop();
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
    const approvals: Array<{
      kind: "approval-status";
      repo: string;
      pr: number;
      sha: string;
    }> = [];
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
      envelope(
        {
          action: "closed",
          issue: { number: 1, updated_at: "2026-01-01T00:00:00.000Z" },
          repository: { full_name: "acme/widgets" },
        },
        "issue-closed"
      )
    );
    await flush();
    state.trees[issue].status = "closed";
    nats.emit(
      "notifications.github.acme.widgets.issue.1.reopened",
      envelope(
        {
          action: "reopened",
          issue: { number: 1, updated_at: "2026-01-01T00:00:01.000Z" },
          repository: { full_name: "acme/widgets" },
        },
        "issue-reopened"
      )
    );
    await flush();
    nats.emit(
      "notifications.github.acme.widgets.pull_request.synchronize",
      envelope(prPayload({ head_sha: "head-2" }), "pr-synchronize")
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
    state.roles[controllerToken(state.project)] = {
      role: "controller",
      sessionId: "ses-controller",
    };
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

  it("forwards a GitHub mention directly to the controller role, without the held-event path", async () => {
    const { state } = stateForIssue();
    state.roles[controllerToken(state.project)] = {
      role: "controller",
      sessionId: "ses-controller",
    };
    const nats = new FakeNats();
    const published: Array<{ topic: string; payloadJson: string }> = [];
    const pump = startEventPump(
      deps(state, nats, async (topic, payloadJson) => {
        published.push({ topic, payloadJson });
      })
    );
    const payloadJson = JSON.stringify({ text: "@legion please investigate" });

    const acks: string[] = [];
    nats.emit(
      "notifications.github.acme.widgets.mention",
      envelope({ text: "@legion please investigate" }, "github-mention-1"),
      () => acks.push("ack-1")
    );
    await flush();

    expect(published).toEqual([{ topic: roleTopic(controllerToken("omp")), payloadJson }]);
    expect(acks).toEqual(["ack-1"]);
    pump.stop();
  });

  it("does not ack, and adds nothing to controllerHeldEvents, when a GitHub mention's publish rejects", async () => {
    const { state } = stateForIssue();
    const nats = new FakeNats();
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump(
      deps(state, nats, async () => {
        throw new Error("listener down");
      })
    );

    try {
      const acks: string[] = [];
      nats.emit(
        "notifications.github.acme.widgets.mention",
        envelope({ text: "@legion please investigate" }, "github-mention-2"),
        () => acks.push("ack-1")
      );

      await expect(pump.drain()).rejects.toThrow();
      expect(acks).toEqual([]);
      expect(state.controllerHeldEvents).toEqual([]);
      // Only `processDurableMessage`'s catch logs a durable-lane rejection;
      // it fires once for this one underlying failure.
      expect(errorLog).toHaveBeenCalledTimes(1);
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });

  it("terms (never retries) a poison durable message that isn't valid JSON, logging the subject and stream/consumer sequence", async () => {
    const { state } = stateForIssue();
    const nats = new FakeNats();
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump(deps(state, nats, async () => {}));

    try {
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        "not JSON",
        { streamSequence: 5, deliverySequence: 2 },
        calls
      );
      await flush();

      expect(calls).toEqual({ acks: 0, naks: [], terms: [expect.any(String)] });
      expect(errorLog).toHaveBeenCalledTimes(1);
      const [message] = errorLog.mock.calls[0] ?? [];
      expect(message).toContain("notifications.github.acme.widgets.issue.1.comment");
      expect(message).toContain("stream_seq=5");
      expect(message).toContain("delivery_seq=2");
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });

  it("terms a durable message that fails envelope schema validation, including its event_id when recoverable", async () => {
    const { state } = stateForIssue();
    const nats = new FakeNats();
    const errorLog = vi.spyOn(console, "error").mockImplementation(() => {});
    const pump = startEventPump(deps(state, nats, async () => {}));

    try {
      const calls: FakeDurableControlCalls = { acks: 0, naks: [], terms: [] };
      // Valid JSON, but missing the envelope's required fields.
      nats.emit(
        "notifications.github.acme.widgets.issue.1.comment",
        JSON.stringify({ event_id: "schema-bad-1" }),
        { streamSequence: 9, deliverySequence: 1 },
        calls
      );
      await flush();

      expect(calls.terms).toHaveLength(1);
      expect(calls.acks).toBe(0);
      expect(calls.naks).toEqual([]);
      const [message] = errorLog.mock.calls[0] ?? [];
      expect(message).toContain("event_id=schema-bad-1");
      expect(message).toContain("stream_seq=9");
    } finally {
      errorLog.mockRestore();
      pump.stop();
    }
  });

  it("runs a runExclusive operation (resync's contract) and a durable message on the same PR strictly sequentially", async () => {
    const { state, issue } = stateForIssue();
    state.prs["acme/widgets#7"] = checkPr(issue);
    const nats = new FakeNats();
    const order: string[] = [];
    const log = vi.spyOn(console, "log").mockImplementation((...args: unknown[]) => {
      if (typeof args[0] === "string" && args[0].includes("consumed event")) {
        order.push("durable-message");
      }
    });
    const pump = startEventPump(deps(state, nats, async () => {}));

    try {
      const gate = Promise.withResolvers<void>();
      const resyncPromise = pump.runExclusive(async () => {
        order.push("resync-start");
        await gate.promise;
        order.push("resync-end");
      });

      // Emitted immediately, before the resync gate is released: if the
      // durable lane weren't serialized against runExclusive, this
      // message would be processed (its "consumed event" log firing)
      // *during* the still-running resync operation instead of after it.
      nats.emit(
        "notifications.github.acme.widgets.pr.7.checks",
        envelope(settledChecks(), "checks-during-resync")
      );
      await flush();
      expect(order).toEqual(["resync-start"]);

      gate.resolve();
      await resyncPromise;
      await flush();

      expect(order).toEqual(["resync-start", "resync-end", "durable-message"]);
    } finally {
      log.mockRestore();
      pump.stop();
    }
  });
});

describe("truncateTermReason", () => {
  it("leaves a reason under the byte cap untouched", () => {
    const reason = "tmux new-window failed";
    expect(truncateTermReason(reason)).toBe(reason);
  });

  it("caps the total UTF-8 byte length at 1024, including the appended ellipsis", () => {
    const reason = "x".repeat(2_000);
    const truncated = truncateTermReason(reason);
    expect(new TextEncoder().encode(truncated).length).toBe(1_024);
    expect(truncated.endsWith("…")).toBe(true);
    expect(truncated).toBe(`${"x".repeat(1_021)}…`);
  });

  it("caps the byte length even when trimming crosses a multi-byte character boundary", () => {
    // "é" is 2 UTF-8 bytes; a naive character-count truncation (1024 chars
    // + a 3-byte ellipsis) would overshoot 1024 bytes here.
    const reason = "é".repeat(2_000);
    const truncated = truncateTermReason(reason);
    const bytes = new TextEncoder().encode(truncated).length;
    expect(bytes).toBeLessThanOrEqual(1_024);
    expect(truncated.endsWith("…")).toBe(true);
  });
});

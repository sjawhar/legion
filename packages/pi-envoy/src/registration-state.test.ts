import { describe, expect, test } from "bun:test";
import { ROLE_TOPIC_PREFIX } from "@legion/contracts";
import { messageFor } from "@legion/envoy-client/errors";
import {
  EnvoyApiError,
  type EnvoyClient,
  type Interest,
  type RoleInfo,
  type SetRoleInput,
  type SetRoleResult,
  type SubscribeInput,
  type UnsubscribeInput,
} from "@legion/envoy-client/transport";
import type { NatsConnection } from "nats";
import {
  createRegistrationState,
  type RegistrationInput,
  type RegistrationState,
  taskWasAbandoned,
} from "./registration-state";

/** The deadline rejection the queue answers with, which the module names through its predicate:
 *  the error class itself is private to it. */
const expectAbandoned = async (running: Promise<unknown>): Promise<void> => {
  await running.then(
    () => {
      throw new Error("the task settled instead of being abandoned at its deadline");
    },
    (error: unknown) => {
      expect(taskWasAbandoned(error)).toBe(true);
    }
  );
};

const ALPHA = `${ROLE_TOPIC_PREFIX}alpha`;
const BETA = `${ROLE_TOPIC_PREFIX}beta`;
const CONTROLLER_NOTICE = "notifications.legion.ACME.controller";

const interestFor = (sessionID: string): Interest => ({
  session_id: sessionID,
  machine_id: "machine-1",
  dir: "/w",
  topics: [],
});

const registrationFor = (sessionID: string): RegistrationInput => ({
  sessionID,
  directory: "/w",
  topics: [`agent.${sessionID}`],
  port: 0,
  title: "a pane",
  driving: false,
  selfSubscribed: true,
  plugin: { name: "@sjawhar/pi-envoy", version: "13.9.0" },
});

const notUsed = (name: string) => (): never => {
  throw new Error(`the fake listener client has no ${name}`);
};

const apiError = (status: number): EnvoyApiError =>
  new EnvoyApiError({
    method: "GET",
    url: "/v1/roles/alpha",
    status,
    responseBody: JSON.stringify({ error: "no live holder" }),
  });

/** What a test moves while a task is parked: the open session, the deadline, each listener call. */
interface HarnessControl {
  /** The one session open here, which the module's `sessionOpen` answers for. */
  open: string;
  deadlineMs: number;
  subscribe: (input: SubscribeInput) => Promise<Interest>;
  setRole: (input: SetRoleInput) => Promise<SetRoleResult>;
  getRole: (role: string) => Promise<RoleInfo>;
  getInterest: (sessionID: string) => Promise<Interest>;
  unsubscribe: (input: UnsubscribeInput) => Promise<void>;
  dial: () => Promise<NatsConnection>;
  /** The session's connection, as the extension's `ensureConnection` answers it: it dials when
   *  the session has none, and can wait again after the dial. */
  connect: (dial: () => Promise<NatsConnection>) => Promise<NatsConnection>;
  subscribeTopic: (topic: string, connect: () => Promise<NatsConnection>) => Promise<boolean>;
}

interface Harness {
  readonly state: RegistrationState;
  readonly control: HarnessControl;
  readonly notices: { readonly message: string; readonly level: string }[];
  readonly warnings: { readonly message: string; readonly context: unknown }[];
  readonly closedSubjects: string[];
  readonly closedConnections: string[];
  readonly roleEntries: string[];
  readonly subscribedTopics: string[];
  readonly registrations: SubscribeInput[];
  /** Each hand-back asked of the listener, as `<topic>@<the session it was held under>`. */
  readonly handBacks: string[];
  readonly deregistrations: {
    readonly sessionID: string;
    readonly clientInstanceID: string | undefined;
  }[];
  readonly claims: SetRoleInput[];
  readonly fakeConnection: (name: string) => NatsConnection;
  /** Claims `role` for `id` through a settled role move, as a healthy pass does. */
  readonly hold: (role: string, id: string) => Promise<boolean | undefined>;
}

function createHarness(): Harness {
  const notices: { readonly message: string; readonly level: string }[] = [];
  const warnings: { readonly message: string; readonly context: unknown }[] = [];
  const closedSubjects: string[] = [];
  const closedConnections: string[] = [];
  const roleEntries: string[] = [];
  const subscribedTopics: string[] = [];
  const registrations: SubscribeInput[] = [];
  const handBacks: string[] = [];
  const deregistrations: {
    readonly sessionID: string;
    readonly clientInstanceID: string | undefined;
  }[] = [];
  const claims: SetRoleInput[] = [];

  const fakeConnection = (name: string): NatsConnection =>
    ({
      close: async (): Promise<void> => {
        closedConnections.push(name);
      },
    }) as unknown as NatsConnection;

  const control: HarnessControl = {
    open: "",
    deadlineMs: 60_000,
    subscribe: async (input) => interestFor(input.sessionID),
    setRole: async () => ({ claimed: true, interest: interestFor("ses_a") }),
    getRole: async (role) => ({ role, holder: "", last_seen: 1, state: "gone" }),
    getInterest: async (sessionID) => interestFor(sessionID),
    unsubscribe: async () => {},
    dial: async () => fakeConnection("one"),
    // The extension's own `ensureConnection` keeps one connection per session; a test that cares
    // about the wait after the dial replaces this.
    connect: (dial) => dial(),
    subscribeTopic: async () => true,
  };

  const client: EnvoyClient = {
    subscribe: async (input) => {
      registrations.push(input);
      return control.subscribe(input);
    },
    unsubscribe: async (input) => {
      for (const topic of input.topics) handBacks.push(`${topic}@${input.sessionID}`);
      await control.unsubscribe(input);
    },
    getInterest: async (sessionID) => control.getInterest(sessionID),
    send: notUsed("send"),
    publish: notUsed("publish"),
    unregisterSession: async (input) => {
      deregistrations.push({
        sessionID: input.sessionID,
        clientInstanceID: input.clientInstanceID,
      });
    },
    setRole: async (input) => {
      claims.push(input);
      return control.setRole(input);
    },
    getRole: async (role) => control.getRole(role),
    listSessions: notUsed("listSessions"),
    replay: notUsed("replay"),
  };

  const state = createRegistrationState({
    client: () => client,
    sessionOpen: (id) => id === control.open,
    deadlineMs: () => control.deadlineMs,
    notify: (message, level) => notices.push({ message, level }),
    log: { warn: (message, context) => warnings.push({ message, context }) },
    appendRoleClaimEntry: (role) => roleEntries.push(role),
    dial: () => control.dial(),
    connect: (dial) => control.connect(dial),
    subscribeTopic: (topic, connect) => {
      subscribedTopics.push(topic);
      return control.subscribeTopic(topic, connect);
    },
    closeSubject: (subject) => closedSubjects.push(subject),
  });

  return {
    state,
    control,
    notices,
    warnings,
    closedSubjects,
    closedConnections,
    roleEntries,
    subscribedTopics,
    registrations,
    handBacks,
    deregistrations,
    claims,
    fakeConnection,
    hold: async (role, id) => {
      control.open = id;
      return state.roleMoves.run(async (handle) => {
        handle.actFor(id);
        return handle.claimRole(role);
      });
    },
  };
}

describe("the serial queues", () => {
  test("runs queued tasks one at a time, in the order they were queued", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    const order: string[] = [];
    const started = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const queued = [
      state.passes.run(async () => {
        order.push("first in");
        started.resolve();
        await release.promise;
        order.push("first out");
        return 1;
      }),
      state.passes.run(async () => {
        order.push("second in");
        return 2;
      }),
    ];
    await started.promise;
    expect(order).toEqual(["first in"]);
    release.resolve();
    expect(await Promise.all(queued)).toEqual([1, 2]);
    expect(order).toEqual(["first in", "first out", "second in"]);
  });

  test("a task that failed does not stop the ones after it", async () => {
    const { state } = createHarness();
    const failing = state.passes.run(async () => {
      throw new Error("the listener said no");
    });
    const after = state.passes.run(async () => "settled");
    await expect(failing).rejects.toThrow("the listener said no");
    expect(await after).toBe("settled");
    await state.passes.settled();
  });

  test("a task names the session it acts for once", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    await expect(
      state.passes.run(async (handle) => {
        handle.actFor("ses_a");
        handle.actFor("ses_b");
        return true;
      })
    ).rejects.toThrow("already acts for session ses_a");
  });

  test("a step of a task past its deadline changes nothing", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    control.deadlineMs = 0;
    const release = Promise.withResolvers<void>();
    const finished = Promise.withResolvers<void>();
    let stepped = false;
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      try {
        await release.promise;
        handle.markEstablished("ses_a");
        stepped = true;
        return true;
      } finally {
        finished.resolve();
      }
    });
    await expectAbandoned(running);
    release.resolve();
    await finished.promise;
    expect(stepped).toBe(false);
    expect(state.establishedFor()).toBe("");
  });

  test("a step of a session task whose session is no longer open changes nothing", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    const release = Promise.withResolvers<void>();
    let stepped = false;
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      await release.promise;
      handle.markRoleSettled("ses_a");
      stepped = true;
      return true;
    });
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(stepped).toBe(false);
    expect(state.roleSettledSession()).toBe("");
  });

  test("a step of a no-session task runs until the deadline", async () => {
    const { state, control } = createHarness();
    // No session of this task's own is open: an unbound task is fenced by its deadline alone.
    control.open = "ses_elsewhere";
    const ran = await state.passes.run(async (handle) => {
      await Promise.resolve();
      handle.markEstablished("ses_unbound");
      return true;
    });
    expect(ran).toBe(true);
    expect(state.establishedFor()).toBe("ses_unbound");

    control.deadlineMs = 0;
    const release = Promise.withResolvers<void>();
    const finished = Promise.withResolvers<void>();
    let stepped = false;
    const running = state.passes.run(async (handle) => {
      try {
        await release.promise;
        handle.markEstablished("ses_late");
        stepped = true;
        return true;
      } finally {
        finished.resolve();
      }
    });
    await expectAbandoned(running);
    release.resolve();
    await finished.promise;
    expect(stepped).toBe(false);
    expect(state.establishedFor()).toBe("ses_unbound");
  });

  test("a pass the host leaves mid-pass decides nothing, with no new warning", async () => {
    const harness = createHarness();
    const { state, control, notices } = harness;
    // The role whose closed notices this pass would report, held and then ended.
    await harness.hold("alpha", "ses_a");
    state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha");
    state.endUnwantedRole("ses_a", null, ["ses_a"]);
    notices.length = 0;

    const release = Promise.withResolvers<void>();
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      await release.promise;
      handle.noteNoticesClosed("alpha");
      handle.markRoleSettled("ses_a");
      return true;
    });
    control.open = "ses_b";
    release.resolve();
    const decided = (await running) ?? false;
    expect(decided).toBe(false);
    expect(notices).toEqual([]);
    expect(state.roleSettledSession()).toBe("");
  });

  test("a dial a task made for a connection its session outran is closed", async () => {
    const { state, control, closedConnections, fakeConnection } = createHarness();
    control.open = "ses_a";
    const dialling = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    control.dial = async () => {
      dialling.resolve();
      await release.promise;
      return fakeConnection("left-behind");
    };
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      await handle.connect();
      return true;
    });
    await dialling.promise;
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(closedConnections).toEqual(["left-behind"]);
  });

  test("a dial a task made for a connection its deadline outran is closed", async () => {
    const { state, control, closedConnections, fakeConnection } = createHarness();
    control.open = "ses_a";
    control.deadlineMs = 0;
    const dialling = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const finished = Promise.withResolvers<void>();
    control.dial = async () => {
      dialling.resolve();
      await release.promise;
      return fakeConnection("abandoned");
    };
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      try {
        await handle.connect();
        return true;
      } finally {
        finished.resolve();
      }
    });
    await dialling.promise;
    await expectAbandoned(running);
    release.resolve();
    await finished.promise;
    expect(closedConnections).toEqual(["abandoned"]);
  });

  test("a subscribe whose dial its task's session outran closes that dial and opens nothing", async () => {
    const { state, control, closedConnections, fakeConnection } = createHarness();
    control.open = "ses_a";
    const dialling = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const opened: string[] = [];
    control.dial = async () => {
      dialling.resolve();
      await release.promise;
      return fakeConnection("subscribe-dial");
    };
    // The adapter opens the topic only once it has a connection, as the extension's own
    // `subscribe` does, and it takes that connection through the step the handle hands it.
    control.subscribeTopic = async (topic, connect) => {
      await connect();
      opened.push(topic);
      return true;
    };
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      return await handle.subscribe("notifications.dispatch.issue.A-1");
    });
    await dialling.promise;
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(opened).toEqual([]);
    expect(closedConnections).toEqual(["subscribe-dial"]);
  });

  // The acquisition of the session's connection waits past its own dial: the extension closes a
  // dial another caller's connection beat and answers with that connection instead. A task
  // refused during that wait must act on neither.
  test("a session that moves while the connection is acquired opens no subject", async () => {
    const { state, control, closedConnections } = createHarness();
    control.open = "ses_a";
    const acquiring = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const opened: string[] = [];
    control.connect = async (dial) => {
      const dialled = await dial();
      acquiring.resolve();
      // The extension's own wait on the connection another caller won the race with.
      await release.promise;
      return dialled;
    };
    control.subscribeTopic = async (topic, connect) => {
      await connect();
      opened.push(topic);
      return true;
    };
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      return await handle.subscribe("notifications.dispatch.issue.A-1");
    });
    await acquiring.promise;
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(opened).toEqual([]);
    // The connection is the session's, not this task's: a refusal leaves it open.
    expect(closedConnections).toEqual([]);
  });

  test("a deadline that fires while the connection is acquired answers the task no connection", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    control.deadlineMs = 0;
    const acquiring = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const finished = Promise.withResolvers<void>();
    let watched = false;
    control.connect = async (dial) => {
      const dialled = await dial();
      acquiring.resolve();
      await release.promise;
      return dialled;
    };
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      try {
        await handle.connect();
        // What the extension does with the connection: replace the agent stream's control
        // subscription. A task the acquisition outran never reaches it.
        watched = true;
        return true;
      } finally {
        finished.resolve();
      }
    });
    await acquiring.promise;
    await expectAbandoned(running);
    release.resolve();
    await finished.promise;
    expect(watched).toBe(false);
  });

  test("a refusal latches: the session opening again does not revive the task", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    const refused = Promise.withResolvers<void>();
    const reopened = Promise.withResolvers<void>();
    let stepped = false;
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      await Promise.resolve();
      // The step the move refuses, caught as `settleRole`'s catch-all catches whatever a step
      // threw.
      try {
        handle.markInboundOpen("ses_a");
      } catch {
        refused.resolve();
      }
      await reopened.promise;
      handle.markRoleSettled("ses_a");
      stepped = true;
      return true;
    });
    control.open = "ses_b";
    await refused.promise;
    // The host switches back: the task is still the one the move replaced.
    control.open = "ses_a";
    reopened.resolve();
    expect(await running).toBeUndefined();
    expect(stepped).toBe(false);
    expect(state.inboundOpenSession()).toBe("");
    expect(state.roleSettledSession()).toBe("");
  });

  test("the recovery's unregister is refused once its task is", async () => {
    const { state, control, handBacks } = createHarness();
    control.open = "ses_a";
    const read = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    control.getInterest = async (sessionID) => {
      read.resolve();
      await release.promise;
      return interestFor(sessionID);
    };
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      await handle.readInterest("ses_a");
      await handle.unregisterTopics("ses_a", ["notifications.slack.channel.C1"], "envoy: nope");
      return true;
    });
    await read.promise;
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(handBacks).toEqual([]);
  });

  test("the handle unregisters a session's topics, and a failure warns in its caller's own words rather than failing the task", async () => {
    const { state, control, handBacks, warnings } = createHarness();
    control.open = "ses_a";
    const unregistered = await state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      await handle.unregisterTopics(
        "ses_a",
        ["notifications.slack.channel.C1"],
        "envoy: could not unregister a resumed controller's retired Slack topics"
      );
      control.unsubscribe = async () => {
        throw apiError(503);
      };
      await handle.unregisterTopics(
        "ses_a",
        ["notifications.slack.channel.C2"],
        "envoy: could not unregister a resumed controller's retired Slack topics"
      );
      return true;
    });
    expect(unregistered).toBe(true);
    expect(handBacks).toEqual([
      "notifications.slack.channel.C1@ses_a",
      "notifications.slack.channel.C2@ses_a",
    ]);
    expect(warnings).toEqual([
      {
        message: "envoy: could not unregister a resumed controller's retired Slack topics",
        context: {
          sessionID: "ses_a",
          topics: ["notifications.slack.channel.C2"],
          error: messageFor(apiError(503)),
        },
      },
    ]);
  });

  test("the handle's listener reads and subscribes answer a task that still decides", async () => {
    const { state, control, subscribedTopics } = createHarness();
    control.open = "ses_a";
    control.getRole = async (role) => ({ role, holder: "ses_a", last_seen: 3, state: "live" });
    const read = await state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      return {
        opened: await handle.subscribe("notifications.dispatch.issue.A-1"),
        interest: (await handle.readInterest("ses_a"))?.session_id,
        holder: await handle.readRoleHolder("alpha"),
      };
    });
    expect(read).toEqual({ opened: true, interest: "ses_a", holder: "ses_a" });
    expect(subscribedTopics).toEqual(["notifications.dispatch.issue.A-1"]);
  });

  test("a failed listener read answers nothing rather than failing the task", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    control.getRole = async () => {
      throw apiError(503);
    };
    control.getInterest = async () => {
      throw apiError(503);
    };
    const read = await state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      return {
        holder: await handle.readRoleHolder("alpha"),
        interest: await handle.readInterest("ses_a"),
      };
    });
    expect(read).toEqual({ holder: undefined, interest: undefined });
  });
});

describe("refreshRegistration", () => {
  test("carries the client instance id, takes this session's abandoned holds back, and times the registration", async () => {
    const harness = createHarness();
    const { state, control, registrations, handBacks } = harness;
    // A hold whose hand-back the listener refused is abandoned under ses_a.
    await harness.hold("alpha", "ses_a");
    state.endUnwantedRole("ses_b", null, ["ses_b"]);
    control.unsubscribe = async () => {
      throw new Error("the listener refused the hand-back");
    };
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
    control.unsubscribe = async () => {};

    const result = await state.refreshRegistration(registrationFor("ses_a"));
    expect(result.session_id).toBe("ses_a");
    expect(registrations).toHaveLength(1);
    expect(registrations[0]?.clientInstanceID).toBe(state.clientInstanceID);
    expect(state.clientInstanceID).toMatch(/^[0-9a-f]{32}$/);
    expect(state.registrationFresherThan("ses_a", 60_000)).toBe(true);

    // The abandoned hold is this process's own again, so the next settle hands it back.
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([`${ALPHA}@ses_a`, `${ALPHA}@ses_a`]);
  });

  test("leaves another session's abandoned hold where it is", async () => {
    const harness = createHarness();
    const { state, control, handBacks } = harness;
    await harness.hold("alpha", "ses_a");
    state.endUnwantedRole("ses_b", null, ["ses_b"]);
    control.unsubscribe = async () => {
      throw new Error("the listener refused the hand-back");
    };
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    control.unsubscribe = async () => {};
    handBacks.length = 0;

    await state.refreshRegistration(registrationFor("ses_other"));
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([]);
  });

  test("a registration the listener refused is timed as no registration at all", async () => {
    const { state, control } = createHarness();
    control.subscribe = async () => {
      throw apiError(503);
    };
    await expect(state.refreshRegistration(registrationFor("ses_a"))).rejects.toBeInstanceOf(
      EnvoyApiError
    );
    expect(state.registrationFresherThan("ses_a", 60_000)).toBe(false);
  });
});

describe("registrationFresherThan", () => {
  test("answers only for the session the listener accepted, and only inside the age", async () => {
    const { state } = createHarness();
    expect(state.registrationFresherThan("ses_a", 60_000)).toBe(false);
    await state.refreshRegistration(registrationFor("ses_a"));
    expect(state.registrationFresherThan("ses_a", 60_000)).toBe(true);
    expect(state.registrationFresherThan("ses_b", 60_000)).toBe(false);
    expect(state.registrationFresherThan("ses_a", 0)).toBe(false);
  });
});

describe("deregister", () => {
  test("sends the deregistration carrying the client instance id", async () => {
    const { state, deregistrations } = createHarness();
    await state.deregister("ses_a");
    expect(deregistrations).toEqual([
      { sessionID: "ses_a", clientInstanceID: state.clientInstanceID },
    ]);
  });
});

describe("clearRefusal", () => {
  test("forgets the role the listener refused to that session", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    control.setRole = async () => ({ claimed: false, holder: "ses_newer" });
    const claimed = await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.claimRole("alpha", { soft: true });
    });
    expect(claimed).toBe(false);
    expect(state.refusalFor("ses_a")).toBe("alpha");
    state.clearRefusal("ses_a");
    expect(state.refusalFor("ses_a")).toBeUndefined();
  });
});

describe("endUnwantedRole", () => {
  test("ends a role the host session's records rule out and leaves its hold pending", async () => {
    const harness = createHarness();
    const { state, control, handBacks } = harness;
    await harness.hold("alpha", "ses_a");
    expect(state.heldRole()).toBe("alpha");

    // A `/new`: the host is on ses_b, whose records name no role and which continues nothing.
    state.endUnwantedRole("ses_b", undefined, ["ses_b"]);
    expect(state.heldRole()).toBeUndefined();
    expect(state.roleEndCount()).toBe(1);

    control.open = "ses_b";
    const restored = await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_b");
      return handle.settleEndedHolds(undefined);
    });
    expect(restored).toBe(false);
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
  });

  test("keeps a role the host session continues, and ends one its records released", async () => {
    const harness = createHarness();
    const { state } = harness;
    await harness.hold("alpha", "ses_a");
    // A fork: ses_b continues ses_a, whose hold the fork carries.
    state.endUnwantedRole("ses_b", undefined, ["ses_b", "ses_a"]);
    expect(state.heldRole()).toBe("alpha");
    // Records that name the same role keep it.
    state.endUnwantedRole("ses_b", "alpha", ["ses_b"]);
    expect(state.heldRole()).toBe("alpha");
    // A hold under the host session itself is carried.
    state.endUnwantedRole("ses_a", undefined, ["ses_a"]);
    expect(state.heldRole()).toBe("alpha");
    // A recorded release ends it.
    state.endUnwantedRole("ses_b", null, ["ses_b", "ses_a"]);
    expect(state.heldRole()).toBeUndefined();
  });

  test("closes the notice subjects a plugin opened for the role it ends", async () => {
    const harness = createHarness();
    const { state, closedSubjects } = harness;
    await harness.hold("alpha", "ses_a");
    state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha");
    state.endUnwantedRole("ses_b", null, ["ses_b"]);
    expect(closedSubjects).toEqual([CONTROLLER_NOTICE]);
    expect(state.isRoleBound(CONTROLLER_NOTICE)).toBe(false);
  });
});

describe("markRoleBoundSubjects and unmarkRoleBoundSubjects", () => {
  test("mark the subjects a plugin takes while the role is held, and unmark the ones that never opened", () => {
    const { state } = createHarness();
    expect(state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha")).toEqual([CONTROLLER_NOTICE]);
    expect(state.isRoleBound(CONTROLLER_NOTICE)).toBe(true);
    expect(state.roleBoundSubjects()).toEqual([CONTROLLER_NOTICE]);
    // One already marked is not marked twice, so a later failure unmarks nothing it did not add.
    expect(state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha")).toEqual([]);
    state.unmarkRoleBoundSubjects([CONTROLLER_NOTICE]);
    expect(state.isRoleBound(CONTROLLER_NOTICE)).toBe(false);
    expect(state.roleBoundSubjects()).toEqual([]);
  });

  test("the marked role is what a pass that claims it back reports closed, once", async () => {
    const harness = createHarness();
    const { state, control, notices } = harness;
    await harness.hold("alpha", "ses_a");
    state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha");
    state.endUnwantedRole("ses_b", null, ["ses_b"]);
    control.open = "ses_a";
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      handle.noteNoticesClosed("alpha");
      // Said once per role the plugin opened notices for: the second call finds it reported.
      handle.noteNoticesClosed("alpha");
      return true;
    });
    expect(notices).toHaveLength(1);
    expect(notices[0]?.message).toContain("role alpha is held again");
    expect(notices[0]?.level).toBe("warning");
  });

  test("nothing is reported while the plugin's subjects are still open", async () => {
    const harness = createHarness();
    const { state, notices } = harness;
    await harness.hold("alpha", "ses_a");
    state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha");
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      handle.noteNoticesClosed("alpha");
      return true;
    });
    expect(notices).toEqual([]);
  });
});

describe("recordClaimAfterDeadline", () => {
  test("leaves the hold the listener granted for the next pass to hand back", async () => {
    const { state, control, handBacks } = createHarness();
    control.open = "ses_a";
    state.recordClaimAfterDeadline(ALPHA, "ses_a");
    expect(state.heldRole()).toBeUndefined();
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
  });

  test("a claim the listener granted after the deadline is recorded, not adopted", async () => {
    const { state, control, handBacks, roleEntries } = createHarness();
    control.open = "ses_a";
    control.deadlineMs = 0;
    const claiming = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const finished = Promise.withResolvers<void>();
    control.setRole = async () => {
      claiming.resolve();
      await release.promise;
      return { claimed: true, interest: interestFor("ses_a") };
    };
    const running = state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      try {
        return await handle.claimRole("alpha", { soft: true });
      } finally {
        finished.resolve();
      }
    });
    await claiming.promise;
    await expectAbandoned(running);
    release.resolve();
    await finished.promise;
    expect(state.heldRole()).toBeUndefined();
    expect(roleEntries).toEqual([]);

    control.deadlineMs = 60_000;
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
  });
});

describe("adoptClaimAfterSessionMoved", () => {
  test("records the new hold and hands the earlier role back at the listener", async () => {
    const harness = createHarness();
    const { state, handBacks } = harness;
    await harness.hold("alpha", "ses_a");
    await state.adoptClaimAfterSessionMoved(BETA, "ses_b", ALPHA, "ses_a");
    expect(state.heldRole()).toBe("beta");
    expect(state.holdsRoleTopic(BETA)).toBe(true);
    expect(state.holdSessionID()).toBe("ses_b");
    expect(state.roleEndCount()).toBe(1);
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
  });

  test("a claim of the role already held keeps that role's notice subjects", async () => {
    const harness = createHarness();
    const { state, handBacks, closedSubjects } = harness;
    await harness.hold("alpha", "ses_a");
    state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha");
    await state.adoptClaimAfterSessionMoved(ALPHA, "ses_b", ALPHA, "ses_a");
    expect(state.holdSessionID()).toBe("ses_b");
    expect(state.roleEndCount()).toBe(0);
    expect(closedSubjects).toEqual([]);
    expect(handBacks).toEqual([]);
  });

  test("a claim that landed after the session moved is adopted under the id the listener names", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    const claiming = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    control.setRole = async () => {
      claiming.resolve();
      await release.promise;
      return { claimed: true, interest: interestFor("ses_a") };
    };
    const refusals: boolean[] = [];
    const running = state.roleMoves.run(
      async (handle) => {
        handle.actFor("ses_a");
        return handle.claimRole("alpha", { soft: true });
      },
      (refusal) => refusals.push(refusal.claimAdopted)
    );
    await claiming.promise;
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(state.heldRole()).toBe("alpha");
    expect(state.holdSessionID()).toBe("ses_a");
    // What the caller tells the model: the claim landed, and the next pass carries it or hands
    // it back.
    expect(refusals).toEqual([true]);
  });

  test("a claim the listener refused while the session moved carries no adoption", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    const claiming = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    control.setRole = async () => {
      claiming.resolve();
      await release.promise;
      return { claimed: false, holder: "ses_newer" };
    };
    const refusals: boolean[] = [];
    const running = state.roleMoves.run(
      async (handle) => {
        handle.actFor("ses_a");
        return handle.claimRole("alpha", { soft: true });
      },
      (refusal) => refusals.push(refusal.claimAdopted)
    );
    await claiming.promise;
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(state.heldRole()).toBeUndefined();
    // Nothing was claimed, so that is what the caller says.
    expect(refusals).toEqual([false]);
  });

  test("a claim whose session moved before its turn carries no adoption and sends nothing", async () => {
    const { state, control, claims } = createHarness();
    control.open = "ses_b";
    const refusals: boolean[] = [];
    const running = state.roleMoves.run(
      async (handle) => {
        handle.actFor("ses_a");
        return handle.claimRole("alpha", { soft: true });
      },
      (refusal) => refusals.push(refusal.claimAdopted)
    );
    expect(await running).toBeUndefined();
    expect(claims).toEqual([]);
    expect(refusals).toEqual([false]);
  });
});

describe("the readers", () => {
  test("name the hold a claim landed, and forget it when the role ends", async () => {
    const harness = createHarness();
    const { state, roleEntries } = harness;
    expect(state.heldRole()).toBeUndefined();
    expect(state.holdSessionID()).toBeUndefined();
    expect(state.holdsRoleTopic(ALPHA)).toBe(false);
    expect(state.holdIsUnconfirmed()).toBe(false);

    await harness.hold("alpha", "ses_a");
    expect(state.heldRole()).toBe("alpha");
    expect(state.holdsRoleTopic(ALPHA)).toBe(true);
    expect(state.holdsRoleTopic(BETA)).toBe(false);
    expect(state.holdSessionID()).toBe("ses_a");
    expect(state.holdIsUnconfirmed()).toBe(false);
    expect(roleEntries).toEqual(["alpha"]);

    state.endUnwantedRole("ses_b", null, ["ses_b"]);
    expect(state.heldRole()).toBeUndefined();
    expect(state.holdsRoleTopic(ALPHA)).toBe(false);
    expect(state.holdSessionID()).toBeUndefined();
  });

  test("roleSnapshot says whether a topic serves the held role, and counts the ends before it", async () => {
    const harness = createHarness();
    const { state } = harness;
    expect(state.roleSnapshot(CONTROLLER_NOTICE)).toEqual({ roleBound: false, endsBefore: 0 });
    await harness.hold("alpha", "ses_a");
    state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha");
    expect(state.roleSnapshot(CONTROLLER_NOTICE)).toEqual({ roleBound: true, endsBefore: 0 });
    // A wildcard expands to its own concrete base and itself, neither of them marked here.
    expect(state.roleSnapshot("notifications.legion.ACME.>")).toEqual({
      roleBound: false,
      endsBefore: 0,
    });
    state.endUnwantedRole("ses_b", null, ["ses_b"]);
    expect(state.roleEndCount()).toBe(1);
    expect(state.roleSnapshot(CONTROLLER_NOTICE)).toEqual({ roleBound: false, endsBefore: 1 });
  });

  test("establishedFor, inboundOpenSession and roleSettledSession follow what a pass recorded", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    expect(state.establishedFor()).toBe("");
    expect(state.inboundOpenSession()).toBe("");
    expect(state.roleSettledSession()).toBe("");
    await state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      handle.markAgentSessionOpen("ses_a");
      handle.markInboundOpen("ses_a");
      handle.markEstablished("ses_a");
      handle.markRoleSettled("ses_a");
      return true;
    });
    expect(state.establishedFor()).toBe("ses_a");
    expect(state.inboundOpenSession()).toBe("ses_a");
    expect(state.roleSettledSession()).toBe("ses_a");
  });

  test("forgetOtherAgentSessions answers the sessions whose agent subject the pass must close", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    await state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      handle.markAgentSessionOpen("ses_a");
      handle.markAgentSessionOpen("ses_old");
      handle.markInboundOpen("ses_old");
      expect(handle.forgetOtherAgentSessions("ses_a")).toEqual(["ses_old"]);
      // The inbound record followed the session it was opened for.
      expect(state.inboundOpenSession()).toBe("");
      // Nothing is left to forget on a second pass.
      expect(handle.forgetOtherAgentSessions("ses_a")).toEqual([]);
      return true;
    });
  });
});

describe("claimRole", () => {
  test("records the hold, writes the transcript entry and hands the earlier role back", async () => {
    const harness = createHarness();
    const { state, handBacks, roleEntries, claims } = harness;
    await harness.hold("alpha", "ses_a");
    await harness.hold("beta", "ses_a");
    expect(state.heldRole()).toBe("beta");
    expect(roleEntries).toEqual(["alpha", "beta"]);
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
    expect(state.roleEndCount()).toBe(1);
    expect(claims.map((claim) => claim.role)).toEqual(["alpha", "beta"]);
  });

  test("a soft claim names the predecessor it continues", async () => {
    const { state, control, claims } = createHarness();
    control.open = "ses_b";
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_b");
      return handle.claimRole("alpha", { soft: true, previousSessionID: "ses_a" });
    });
    expect(claims).toEqual([
      { sessionID: "ses_b", role: "alpha", soft: true, previousSessionID: "ses_a" },
    ]);
  });

  test("a refused claim ends the role and records the refusal", async () => {
    const harness = createHarness();
    const { state, control, warnings } = harness;
    await harness.hold("alpha", "ses_a");
    control.setRole = async () => ({ claimed: false, holder: "ses_newer" });
    const claimed = await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.claimRole("alpha", { soft: true });
    });
    expect(claimed).toBe(false);
    expect(state.heldRole()).toBeUndefined();
    expect(state.refusalFor("ses_a")).toBe("alpha");
    expect(warnings.at(-1)?.message).toBe(
      "envoy: role held by another live session; not reclaimed"
    );
  });

  test("a claim the listener refused after the task's deadline records nothing at all", async () => {
    const { state, control, warnings } = createHarness();
    control.open = "ses_a";
    control.deadlineMs = 0;
    const claiming = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const finished = Promise.withResolvers<void>();
    control.setRole = async () => {
      claiming.resolve();
      await release.promise;
      return { claimed: false, holder: "ses_newer" };
    };
    const running = state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      try {
        return await handle.claimRole("alpha", { soft: true });
      } finally {
        finished.resolve();
      }
    });
    await claiming.promise;
    await expectAbandoned(running);
    release.resolve();
    await finished.promise;
    expect(state.refusalFor("ses_a")).toBeUndefined();
    expect(warnings).toEqual([]);
  });

  test("a refusal the task's deadline outran while the hold was read is not recorded, and the line says so", async () => {
    const { state, control, warnings } = createHarness();
    control.open = "ses_a";
    // A hold of alpha under ses_old that the listener refused to take back.
    control.unsubscribe = async () => {
      throw new Error("the listener refused the hand-back");
    };
    state.recordClaimAfterDeadline(ALPHA, "ses_old");
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    warnings.length = 0;

    // ses_old is still the holder the listener names, so the refused claim reads its `last_seen`;
    // the move is abandoned at its deadline with that read in flight.
    control.setRole = async () => ({ claimed: false, holder: "ses_old" });
    const reading = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const finished = Promise.withResolvers<void>();
    control.getRole = async (role) => {
      reading.resolve();
      await release.promise;
      return { role, holder: "ses_old", last_seen: 9, state: "live" };
    };
    control.deadlineMs = 0;
    const running = state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      try {
        return await handle.claimRole("alpha", { soft: true });
      } finally {
        finished.resolve();
      }
    });
    await reading.promise;
    await expectAbandoned(running);
    release.resolve();
    await finished.promise;
    expect(state.refusalFor("ses_a")).toBeUndefined();
    expect(warnings.at(-1)?.message).toBe(
      "envoy: role held by another live session; the move was abandoned at its deadline before the refusal was recorded"
    );
  });

  test("a claim that landed clears the refusal and the holds of that role", async () => {
    const { state, control, handBacks } = createHarness();
    control.open = "ses_a";
    control.setRole = async () => ({ claimed: false, holder: "ses_newer" });
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.claimRole("alpha", { soft: true });
    });
    expect(state.refusalFor("ses_a")).toBe("alpha");
    // A hold of the same role this process gave up earlier.
    state.recordClaimAfterDeadline(ALPHA, "ses_a");
    control.setRole = async () => ({ claimed: true, interest: interestFor("ses_a") });
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.claimRole("alpha", { soft: true });
    });
    expect(state.refusalFor("ses_a")).toBeUndefined();
    expect(state.holdIsUnconfirmed()).toBe(false);
    // The listener names this session for the role now, so the pending hold is gone rather than
    // handed back: handing it back would end the new hold.
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([]);
  });

  test("a task that named no session cannot claim", async () => {
    const { state, control } = createHarness();
    control.open = "ses_a";
    await expect(state.roleMoves.run(async (handle) => handle.claimRole("alpha"))).rejects.toThrow(
      "has not named the session it acts for"
    );
  });
});

describe("reassertRole", () => {
  const reassert = (harness: Harness, afterOutage: boolean): Promise<unknown> =>
    harness.state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.reassertRole("alpha", afterOutage);
    });

  test("a healthy tick the listener still names writes nothing and asks for no regain", async () => {
    const harness = createHarness();
    await harness.hold("alpha", "ses_a");
    harness.control.getRole = async (role) => ({
      role,
      holder: "ses_a",
      last_seen: 2,
      state: "live",
    });
    harness.claims.length = 0;
    expect(await reassert(harness, false)).toEqual({ decided: true });
    expect(harness.claims).toEqual([]);
    expect(harness.state.heldRole()).toBe("alpha");
  });

  test("the first healthy tick after an outage asks for a reregistered regain", async () => {
    const harness = createHarness();
    await harness.hold("alpha", "ses_a");
    harness.control.getRole = async (role) => ({
      role,
      holder: "ses_a",
      last_seen: 2,
      state: "live",
    });
    expect(await reassert(harness, true)).toEqual({ decided: true, regain: "reregistered" });
  });

  test("a claim the listener lost is re-asserted and asks for a reclaimed regain", async () => {
    const harness = createHarness();
    await harness.hold("alpha", "ses_a");
    // 404 is the listener's answer for "no live holder", not a failure.
    harness.control.getRole = async () => {
      throw apiError(404);
    };
    harness.claims.length = 0;
    expect(await reassert(harness, false)).toEqual({ decided: true, regain: "reclaimed" });
    expect(harness.claims).toEqual([{ sessionID: "ses_a", role: "alpha", soft: true }]);
    expect(harness.state.heldRole()).toBe("alpha");
  });

  test("a refused re-assertion ends the role, records it and tells the pane", async () => {
    const harness = createHarness();
    await harness.hold("alpha", "ses_a");
    harness.control.getRole = async (role) => ({
      role,
      holder: "ses_other",
      last_seen: 2,
      state: "live",
    });
    harness.control.setRole = async () => ({ claimed: false, holder: "ses_newer" });
    harness.notices.length = 0;
    expect(await reassert(harness, false)).toEqual({ decided: true });
    expect(harness.state.heldRole()).toBeUndefined();
    expect(harness.state.refusalFor("ses_a")).toBe("alpha");
    expect(harness.notices).toEqual([
      {
        message:
          "envoy: role alpha is now held by session ses_newer; this session no longer holds it",
        level: "warning",
      },
    ]);
  });

  test("a read that failed for any other reason reaches the caller", async () => {
    const harness = createHarness();
    await harness.hold("alpha", "ses_a");
    harness.control.getRole = async () => {
      throw apiError(503);
    };
    await expect(reassert(harness, false)).rejects.toBeInstanceOf(EnvoyApiError);
  });

  test("decides nothing when the hold moved off the role while the listener was read", async () => {
    const harness = createHarness();
    const { state, control } = harness;
    await harness.hold("alpha", "ses_a");
    const reading = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    control.getRole = async (role) => {
      reading.resolve();
      await release.promise;
      return { role, holder: "ses_other", last_seen: 2, state: "live" };
    };
    const running = state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.reassertRole("alpha", false);
    });
    await reading.promise;
    // A session event ends the role while the read is in flight.
    state.endUnwantedRole("ses_a", null, ["ses_a"]);
    release.resolve();
    expect(await running).toEqual({ decided: false });
    expect(harness.claims.map((claim) => claim.role)).toEqual(["alpha"]);
  });
});

describe("settleEndedHolds", () => {
  test("adopts the wanted hold under a session this process still registers", async () => {
    const { state, control, handBacks } = createHarness();
    control.open = "ses_a";
    state.recordClaimAfterDeadline(ALPHA, "ses_a");
    state.recordClaimAfterDeadline(BETA, "ses_a");
    const restored = await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds("alpha");
    });
    expect(restored).toBe(true);
    expect(state.heldRole()).toBe("alpha");
    expect(state.holdSessionID()).toBe("ses_a");
    // Unconfirmed: nothing has checked it at the listener since.
    expect(state.holdIsUnconfirmed()).toBe(true);
    expect(handBacks).toEqual([`${BETA}@ses_a`]);
  });

  test("never adopts a hold under a session this process has left", async () => {
    const { state, control, handBacks } = createHarness();
    control.open = "ses_b";
    state.recordClaimAfterDeadline(ALPHA, "ses_a");
    const restored = await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_b");
      return handle.settleEndedHolds("alpha");
    });
    expect(restored).toBe(false);
    expect(state.heldRole()).toBeUndefined();
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
  });

  test("abandons a hold the listener refused to take back, until the session registers again", async () => {
    const { state, control, handBacks, warnings } = createHarness();
    control.open = "ses_a";
    control.unsubscribe = async () => {
      throw new Error("the listener refused the hand-back");
    };
    state.recordClaimAfterDeadline(ALPHA, "ses_a");
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(warnings.at(-1)?.message).toBe("envoy: a role this process gave up was not handed back");
    control.unsubscribe = async () => {};
    handBacks.length = 0;
    // Abandoned, so the next settle finds nothing: only a registration takes it back.
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([]);
    await state.refreshRegistration(registrationFor("ses_a"));
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
  });

  test("a settle whose session moved mid-hand-back decides nothing and keeps what is left", async () => {
    const { state, control, handBacks } = createHarness();
    control.open = "ses_a";
    state.recordClaimAfterDeadline(ALPHA, "ses_a");
    state.recordClaimAfterDeadline(BETA, "ses_a");
    const handingBack = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    let handedBack = 0;
    control.unsubscribe = async () => {
      handedBack += 1;
      if (handedBack > 1) return;
      handingBack.resolve();
      await release.promise;
    };
    const running = state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    await handingBack.promise;
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);

    // The hold the loop stopped before is still pending for the move that replaces this one.
    control.open = "ses_a";
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.settleEndedHolds(undefined);
    });
    expect(handBacks).toEqual([`${ALPHA}@ses_a`, `${BETA}@ses_a`]);
  });
});

describe("handBackHeldRole", () => {
  test("ends the role, hands its hold back and answers what it ended", async () => {
    const harness = createHarness();
    const { state, handBacks, closedSubjects } = harness;
    await harness.hold("alpha", "ses_a");
    state.markRoleBoundSubjects([CONTROLLER_NOTICE], "alpha");
    const ended = await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.handBackHeldRole(false);
    });
    expect(ended).toEqual({ role: "alpha", pluginHeld: true });
    expect(state.heldRole()).toBeUndefined();
    expect(closedSubjects).toEqual([CONTROLLER_NOTICE]);
    expect(handBacks).toEqual([`${ALPHA}@ses_a`]);
    expect(harness.notices).toEqual([]);
  });

  test("tells the pane when the end followed a move no session event announced", async () => {
    const harness = createHarness();
    const { state, notices } = harness;
    await harness.hold("alpha", "ses_a");
    await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.handBackHeldRole(true);
    });
    expect(notices).toHaveLength(1);
    expect(notices[0]?.message).toContain("envoy: role alpha ended");
    expect(notices[0]?.message).toContain("run envoy_role_set");
  });

  test("answers nothing when this process holds no role", async () => {
    const { state, control, handBacks } = createHarness();
    control.open = "ses_a";
    const ended = await state.roleMoves.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.handBackHeldRole(true);
    });
    expect(ended).toBeUndefined();
    expect(handBacks).toEqual([]);
  });
});

describe("the handle's register step", () => {
  test("registers through the same operation, so the registration clock is one", async () => {
    const { state, control, registrations } = createHarness();
    control.open = "ses_a";
    const interest = await state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      return handle.register(registrationFor("ses_a"));
    });
    expect(interest?.session_id).toBe("ses_a");
    expect(registrations[0]?.clientInstanceID).toBe(state.clientInstanceID);
    expect(state.registrationFresherThan("ses_a", 60_000)).toBe(true);
  });

  test("a registration a refused task asked for is never sent", async () => {
    const { state, control, registrations } = createHarness();
    control.open = "ses_a";
    const release = Promise.withResolvers<void>();
    const running = state.passes.run(async (handle) => {
      handle.actFor("ses_a");
      await release.promise;
      return handle.register(registrationFor("ses_a"));
    });
    control.open = "ses_b";
    release.resolve();
    expect(await running).toBeUndefined();
    expect(registrations).toEqual([]);
    expect(state.registrationFresherThan("ses_a", 60_000)).toBe(false);
  });
});

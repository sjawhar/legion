import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LegionReadyRequest } from "@legion/contracts/legion-api";
import {
  createLegionDaemonClient,
  daemonUnreachable,
  LegionDaemonApiError,
  type LegionDaemonClient,
  LegionDaemonContractError,
  type ReadyInput,
  retryWhileDaemonRestarts,
} from "./daemon-client";

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

/** `body` without `key`: what a daemon that dropped a field the contract requires would answer. */
function without(body: Record<string, unknown>, key: string): Record<string, unknown> {
  return Object.fromEntries(Object.entries(body).filter(([name]) => name !== key));
}

interface Recorded {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

function daemon(respond: (url: URL) => Response): {
  readonly fetch: typeof fetch;
  readonly requests: Recorded[];
} {
  const requests: Recorded[] = [];
  const stub = (async (input, init) => {
    const url = new URL(input.toString());
    requests.push({
      method: init?.method ?? "GET",
      url: url.toString(),
      body: init?.body == null ? undefined : JSON.parse(init.body.toString()),
    });
    return respond(url);
  }) as typeof fetch;
  return { fetch: stub, requests };
}

const registration = {
  bootToken: "boot-token-from-the-pane",
  sessionId: "ses_architect_208",
  ompSessionFile: "/state/home/.omp/agent/sessions/ses_architect_208.jsonl",
  agentId: "ses_architect_208",
  pluginContract: 1,
};

test("registers with the claim wire's request and returns the claim the daemon issued", async () => {
  const issued = daemonFixture("register.json");
  const { fetch, requests } = daemon(() => Response.json(issued));
  const client = createLegionDaemonClient("http://daemon.test/", fetch);

  await expect(client.register(registration)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: registration },
  ]);
});

test("refuses a registration answer that drops a field the daemon API contract requires, without quoting its secret", async () => {
  const registered = daemonFixture("register.json");
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(without(registered, "promptAgents"))).fetch
  );

  const refused = client.register(registration);
  await expect(refused).rejects.toBeInstanceOf(LegionDaemonContractError);
  await expect(refused).rejects.toThrow("POST /legion/v1/claims/register answered 200");
  await refused.catch((error: Error) => {
    expect(error.message).toContain("promptAgents");
    expect(error.message).not.toContain(String(registered.secret));
  });
});

test("a refusal carries its status and the daemon's sentence", async () => {
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(daemonFixture("error.json"), { status: 409 })).fetch
  );

  const refused = await client.register(registration).catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({
    status: 409,
    detail: "Worker respawn must resume the same agent session",
    message:
      "POST /legion/v1/claims/register failed with 409: Worker respawn must resume the same agent session",
  });
});

test("a refusal whose body is not the daemon's refusal shape keeps its status and says so", async () => {
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => new Response("upstream timed out", { status: 504 })).fetch
  );

  const refused = await client.register(registration).catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({ status: 504 });
  expect((refused as LegionDaemonApiError).detail).toContain("upstream timed out");
  expect((refused as LegionDaemonApiError).detail).toContain("not the daemon's refusal");
});

test("ready and exit post the claim wire's requests and accept only the empty 204", async () => {
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", fetch);
  const ready = {
    claimToken: "legion-legion-legion-208-architect",
    sessionId: "ses_architect_208",
    secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
    generation: 3,
  };

  await expect(client.ready(ready)).resolves.toBeUndefined();
  await expect(client.exit({ ...ready, reason: "session ended" })).resolves.toBeUndefined();
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/ready", body: ready },
    {
      method: "POST",
      url: "http://daemon.test/legion/v1/claims/exit",
      body: { ...ready, reason: "session ended" },
    },
  ]);

  const talkative = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({})).fetch
  );
  await expect(talkative.ready(ready)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("activity posts the claim wire's report and accepts only the empty 204", async () => {
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", fetch);
  const report = {
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: "ses_implementer_208",
    secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
    activity: {
      at: "2026-11-01T12:00:00.000Z",
      backgroundWork: { running: true, kinds: ["async-job"] },
      phase: { nextReminderAt: "2026-11-01T12:05:00.000Z" },
    },
  };

  await expect(client.activity(report)).resolves.toBeUndefined();
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/activity", body: report },
  ]);

  const talkative = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({})).fetch
  );
  await expect(talkative.activity(report)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("the release records are read by a POST carrying the claim's token and secret, and a list the daemon left out is refused", async () => {
  const records = {
    releases: [
      {
        id: 1,
        issue: "LEGION-208",
        generation: 2,
        roles: ["implementer", "tester"],
        at: "2026-11-01T12:10:00Z",
        bundleBytes: 2048,
      },
    ],
    resumes: [
      {
        id: 1,
        release: 1,
        issue: "LEGION-208",
        role: "implementer",
        kind: "task",
        at: "2026-11-01T12:40:00Z",
        podReadyAt: "2026-11-01T12:41:00Z",
        firstTurnAt: "2026-11-01T12:42:00Z",
        stuck: false,
      },
    ],
    holds: [
      {
        id: 1,
        issue: "LEGION-209",
        reason: "background_work",
        detail: "async-job",
        firstAt: "2026-11-01T12:50:00Z",
        at: "2026-11-01T13:00:00Z",
        passes: 11,
      },
    ],
  };
  const { fetch, requests } = daemon(() => Response.json(records));
  const client = createLegionDaemonClient("http://daemon.test", fetch);
  // The pod holds its claim and no operator token, so the claim's own token and secret
  // authenticate the read, and the secret rides in the body rather than a query string.
  const window = {
    claimToken: "legion-legion-legion-208-implementer",
    secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
    since: "2026-11-01T12:00:00Z",
  };

  await expect(client.releaseRecords(window)).resolves.toEqual(records as never);
  expect(requests).toEqual([
    {
      method: "POST",
      url: "http://daemon.test/legion/v1/claims/release-records",
      body: window,
    },
  ]);

  // A window with nothing in it is three empty lists; a list the daemon left out is no answer.
  const empty = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({ releases: [], resumes: [], holds: [] })).fetch
  );
  await expect(empty.releaseRecords(window)).resolves.toEqual({
    releases: [],
    resumes: [],
    holds: [],
  });
  const partial = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({ releases: [], resumes: [] })).fetch
  );
  await expect(partial.releaseRecords(window)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("a ready carrying the capability report is the daemon's golden ReadyRequest, field for field", async () => {
  // ready.json is what the daemon's golden test wrote of a `claim.ReadyRequest` with a report, so
  // sending it as the client's input and recording the body it posts pins both sides to it.
  const fixture = daemonFixture("ready.json");
  const golden: ReadyInput = LegionReadyRequest.parse(fixture);
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", fetch);

  await expect(client.ready(golden)).resolves.toBeUndefined();
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/ready", body: fixture },
  ]);
  expect(golden.capabilities?.rows).toHaveLength(3);
});

test("reads the daemon's state over a bodyless GET", async () => {
  const state = daemonFixture("state.json");
  const { fetch, requests } = daemon(() => Response.json(state));

  await expect(createLegionDaemonClient("http://daemon.test", fetch).state()).resolves.toEqual(
    state as never
  );
  expect(requests).toEqual([
    { method: "GET", url: "http://daemon.test/legion/v1/state", body: undefined },
  ]);
});

test("a state read hands fetch its caller's signal, and an abort ends a read the daemon never answers", async () => {
  const handed: (AbortSignal | null | undefined)[] = [];
  const hung = (async (_input, init) => {
    handed.push(init?.signal);
    const { promise, reject } = Promise.withResolvers<Response>();
    init?.signal?.addEventListener("abort", () => reject(init.signal?.reason), { once: true });
    return promise;
  }) as typeof fetch;
  const controller = new AbortController();
  const late = new Error("the reader stopped waiting");

  const read = createLegionDaemonClient("http://daemon.test", hung).state({
    signal: controller.signal,
  });
  controller.abort(late);

  await expect(read).rejects.toBe(late);
  expect(handed).toEqual([controller.signal]);
});

test("posts every Stage 3 workflow request through its matching route", async () => {
  type WorkflowClient = {
    readonly grant: (body: object) => Promise<unknown>;
    readonly handoffComplete: (body: object) => Promise<unknown>;
    readonly issueStatus: (body: object) => Promise<unknown>;
    readonly gateRegister: (body: object) => Promise<unknown>;
    readonly waveRelease: (body: object) => Promise<unknown>;
    readonly phaseBackward: (body: object) => Promise<unknown>;
    readonly phaseRetry: (body: object) => Promise<unknown>;
    readonly signOff: (body: object) => Promise<unknown>;
    readonly childPark: (body: object) => Promise<unknown>;
    readonly childRerun: (body: object) => Promise<unknown>;
  };
  const calls: ReadonlyArray<readonly [keyof WorkflowClient, string, object, string]> = [
    [
      "grant",
      "/legion/v1/grants",
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-209" },
      "grant.json",
    ],
    // An implementing round's completion names the Dispatch handoff record and the pushed tip,
    // which the legion tool found in the pane; the daemon answers the note READY leaves for a pull
    // request already merged.
    [
      "handoffComplete",
      "/legion/v1/handoff/complete",
      {
        grantId: "grant-208",
        summary: "done",
        verdict: "",
        ready: false,
        commit: "abc123",
        handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
        handoffVersion: 2,
      },
      "handoff-complete.json",
    ],
    [
      "issueStatus",
      "/legion/v1/issues/status",
      { grantId: "grant-208", issue: "LEGION-208", status: "todo" },
      "issue-status.json",
    ],
    [
      "gateRegister",
      "/legion/v1/gates/register",
      {
        grantId: "grant-208",
        issue: "LEGION-208",
        artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
        version: 7,
      },
      "gate-register.json",
    ],
    [
      "waveRelease",
      "/legion/v1/waves/release",
      { grantId: "grant-208", issues: ["LEGION-209"] },
      "wave-release.json",
    ],
    [
      "phaseBackward",
      "/legion/v1/phase/backward",
      { grantId: "grant-208", to: "implementing", reason: "test failed" },
      "phase-backward.json",
    ],
    [
      "phaseRetry",
      "/legion/v1/phase/retry",
      { grantId: "grant-208", issue: "LEGION-208", decision: "retry" },
      "phase-retry.json",
    ],
    [
      "signOff",
      "/legion/v1/signoff",
      { grantId: "grant-208", issue: "LEGION-208" },
      "signoff.json",
    ],
    [
      "childPark",
      "/legion/v1/children/park",
      { grantId: "grant-208", issue: "LEGION-209" },
      "child-park.json",
    ],
    [
      "childRerun",
      "/legion/v1/children/rerun",
      { grantId: "grant-208", issue: "LEGION-209" },
      "child-rerun.json",
    ],
  ];
  const { fetch, requests } = daemon((url) => {
    const call = calls.find(([, route]) => route === url.pathname);
    return Response.json(call === undefined ? { error: "no route" } : daemonFixture(call[3]));
  });
  const client = createLegionDaemonClient("http://daemon.test", fetch) as unknown as WorkflowClient;

  for (const [operation, route, body, fixture] of calls) {
    expect(client[operation], `${operation} is available`).toBeDefined();
    await expect(client[operation](body)).resolves.toEqual(daemonFixture(fixture));
    expect(requests.at(-1)).toEqual({
      method: "POST",
      url: `http://daemon.test${route}`,
      body,
    });
  }
});

test("a workflow refusal surfaces its stable code and message", async () => {
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() =>
      Response.json(
        { code: "GATE_UNAPPROVED", error: "the current spec version needs approval" },
        { status: 409 }
      )
    ).fetch
  ) as unknown as {
    readonly gateRegister?: (body: object) => Promise<unknown>;
  };

  expect(client.gateRegister, "gateRegister is available").toBeDefined();
  if (client.gateRegister === undefined) return;
  const refused = await client
    .gateRegister({
      grantId: "grant-208",
      issue: "LEGION-208",
      artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
      version: 7,
    })
    .catch((error: unknown) => error);
  expect(refused).toMatchObject({
    code: "GATE_UNAPPROVED",
    detail: "the current spec version needs approval",
    message:
      "POST /legion/v1/gates/register failed with 409 GATE_UNAPPROVED: the current spec version needs approval",
  });
});

test("a controller registers on the claim route with its capability and reads the controller's answer", async () => {
  const issued = daemonFixture("register-controller.json");
  const { fetch, requests } = daemon(() => Response.json(issued));
  const client = createLegionDaemonClient("http://daemon.test", fetch);
  const capability = { ...registration, bootToken: "controller-capability" };

  await expect(client.registerController(capability)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: capability },
  ]);
  // A claim's registration is not a controller's, nor the other way round.
  const claimAnswer = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(daemonFixture("register.json"))).fetch
  );
  await expect(claimAnswer.registerController(capability)).rejects.toBeInstanceOf(
    LegionDaemonContractError
  );
  await expect(
    createLegionDaemonClient("http://daemon.test", fetch).register(registration)
  ).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("the registered controller mints its grant with the session form that names no tree", async () => {
  const { fetch, requests } = daemon(() => Response.json(daemonFixture("grant.json")));
  const client = createLegionDaemonClient("http://daemon.test", fetch);

  await expect(
    client.controllerGrant({ sessionId: "ses_controller", secret: "registration-secret" })
  ).resolves.toEqual(daemonFixture("grant.json") as never);
  expect(requests).toEqual([
    {
      method: "POST",
      url: "http://daemon.test/legion/v1/grants",
      body: { sessionId: "ses_controller", secret: "registration-secret" },
    },
  ]);
});

test("the registration, the grant and the handoff completion are read whole when each carries a key no schema names, and the key is not in the value", async () => {
  const registered = daemonFixture("register.json");
  const granted = daemonFixture("grant.json");
  const completed = daemonFixture("handoff-complete.json");
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon((url) => {
      if (url.pathname === "/legion/v1/claims/register")
        return Response.json({ ...registered, roleTokens: { reviewer: "t" } });
      if (url.pathname === "/legion/v1/grants") return Response.json({ ...granted, scope: "repo" });
      return Response.json({ ...completed, mergeQueuePosition: 2 });
    }).fetch
  );

  const claim = await client.register(registration);
  expect(claim).toEqual(registered as never);
  expect(claim).not.toHaveProperty("roleTokens");

  const grant = await client.grant({
    sessionId: "ses_208",
    secret: "claim-secret",
    tree: "LEGION-208",
    issue: "LEGION-209",
  });
  expect(grant).toEqual(granted as never);
  expect(grant).not.toHaveProperty("scope");

  const completion = await client.handoffComplete({
    grantId: "grant-208",
    summary: "done",
    verdict: "",
    ready: false,
    commit: "abc123",
    handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
    handoffVersion: 2,
  });
  expect(completion).toEqual(completed as never);
  expect(completion).not.toHaveProperty("mergeQueuePosition");
});

test("a body that drops a required field, or carries one of a type the contract does not describe, is refused by the path of the field that is wrong", async () => {
  const state = daemonFixture("state.json");
  const issues = state.issues as Record<string, Record<string, unknown>>;

  const dropped = await createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(without(state, "capabilities"))).fetch
  )
    .state()
    .catch((error: unknown) => error);
  expect(dropped).toBeInstanceOf(LegionDaemonContractError);
  expect((dropped as Error).message).toContain("capabilities");

  const mistyped = await createLegionDaemonClient(
    "http://daemon.test",
    daemon(() =>
      Response.json({
        ...state,
        issues: { ...issues, "LEGION-208": { ...issues["LEGION-208"], generation: "three" } },
      })
    ).fetch
  )
    .state()
    .catch((error: unknown) => error);
  expect(mistyped).toBeInstanceOf(LegionDaemonContractError);
  expect((mistyped as Error).message).toContain("issues.LEGION-208.generation");
});

/** A daemon whose every route refuses with that status, that code, and a key no schema names. */
function refusing(status: number, code: string): LegionDaemonClient {
  return createLegionDaemonClient(
    "http://daemon.test",
    daemon(() =>
      Response.json(
        { code, error: "Dispatch refused the write", retryAfterSeconds: 30 },
        { status }
      )
    ).fetch
  );
}

test("a 409 refusal keeps its stable code through a key no schema names", async () => {
  const refused = await refusing(409, "GATE_UNAPPROVED")
    .state()
    .catch((error: unknown) => error);

  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({
    status: 409,
    code: "GATE_UNAPPROVED",
    detail: "Dispatch refused the write",
  });
});

test("a 502 refusal keeps its stable code through a key no schema names, and is the daemon's own answer, which is handed back rather than retried for ten minutes", async () => {
  const client = refusing(502, "DISPATCH_FAILED");
  const refused = await client.state().catch((error: unknown) => error);

  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({ status: 502, code: "DISPATCH_FAILED" });
  // A 502 the daemon itself coded is not a restarting daemon, so the retry hands it straight
  // back: a worker is told why it was refused instead of waiting out the restart budget.
  expect(daemonUnreachable(refused)).toBeUndefined();
  let clock = 0;
  const waits: number[] = [];
  let attempts = 0;
  const retried = await retryWhileDaemonRestarts(
    "read the state",
    async () => {
      attempts += 1;
      return client.state();
    },
    {
      now: () => clock,
      sleep: async (ms: number) => {
        waits.push(ms);
        clock += ms;
      },
    }
  ).catch((error: unknown) => error);
  expect(retried).toBeInstanceOf(LegionDaemonApiError);
  expect(retried).toMatchObject({ status: 502, code: "DISPATCH_FAILED" });
  expect(attempts).toBe(1);
  expect(waits).toEqual([]);
});

import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import {
  createLegionGoDaemonClient,
  LegionGoDaemonApiError,
  LegionGoDaemonContractError,
} from "./go-daemon-client";

/** A response the Go daemon's own golden test wrote (`packages/daemon-go/internal/api`). */
function goFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(
      path.resolve(import.meta.dir, "../../../contracts/fixtures/daemon-api", name),
      "utf8"
    )
  );
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
  const issued = goFixture("register.json");
  const { fetch, requests } = daemon(() => Response.json(issued));
  const client = createLegionGoDaemonClient("http://daemon.test/", fetch);

  await expect(client.register(registration)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: registration },
  ]);
});

test("refuses a registration answer the Go contract does not describe, without quoting its secret", async () => {
  const registered = goFixture("register.json");
  const issued = { ...registered, roleTokens: {} };
  const client = createLegionGoDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(issued)).fetch
  );

  const refused = client.register(registration);
  await expect(refused).rejects.toBeInstanceOf(LegionGoDaemonContractError);
  await expect(refused).rejects.toThrow("POST /legion/v1/claims/register answered 200");
  await refused.catch((error: Error) => {
    expect(error.message).toContain("roleTokens");
    expect(error.message).not.toContain(String(registered.secret));
  });
});

test("a refusal carries its status and the daemon's sentence", async () => {
  const client = createLegionGoDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(goFixture("error.json"), { status: 409 })).fetch
  );

  const refused = await client.register(registration).catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(LegionGoDaemonApiError);
  expect(refused).toMatchObject({
    status: 409,
    detail: "Worker respawn must resume the same agent session",
    message:
      "POST /legion/v1/claims/register failed with 409: Worker respawn must resume the same agent session",
  });
});

test("a refusal whose body is not the Go refusal shape keeps its status and says so", async () => {
  const client = createLegionGoDaemonClient(
    "http://daemon.test",
    daemon(() => new Response("upstream timed out", { status: 504 })).fetch
  );

  const refused = await client.register(registration).catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(LegionGoDaemonApiError);
  expect(refused).toMatchObject({ status: 504 });
  expect((refused as LegionGoDaemonApiError).detail).toContain("upstream timed out");
  expect((refused as LegionGoDaemonApiError).detail).toContain("not the Go daemon's refusal");
});

test("ready and exit post the claim wire's requests and accept only the empty 204", async () => {
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionGoDaemonClient("http://daemon.test", fetch);
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

  const talkative = createLegionGoDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({})).fetch
  );
  await expect(talkative.ready(ready)).rejects.toBeInstanceOf(LegionGoDaemonContractError);
});

test("reads the Go daemon's state strictly, refusing the TypeScript daemon's shape", async () => {
  const goState = goFixture("state.json");
  const { fetch, requests } = daemon(() => Response.json(goState));

  await expect(createLegionGoDaemonClient("http://daemon.test", fetch).state()).resolves.toEqual(
    goState as never
  );
  expect(requests).toEqual([
    { method: "GET", url: "http://daemon.test/legion/v1/state", body: undefined },
  ]);

  const typescriptState = daemon(() =>
    Response.json({ ...goState, workerAdmission: { queue: [] } })
  ).fetch;
  await expect(
    createLegionGoDaemonClient("http://daemon.test", typescriptState).state()
  ).rejects.toBeInstanceOf(LegionGoDaemonContractError);
});

test("posts every Stage 3 workflow request through its matching Go route", async () => {
  type WorkflowClient = {
    readonly grant: (body: object) => Promise<unknown>;
    readonly githubToken: (body: object) => Promise<unknown>;
    readonly gitCredential: (body: object) => Promise<unknown>;
    readonly provisioningCredential: (body: object) => Promise<unknown>;
    readonly handoffComplete: (body: object) => Promise<unknown>;
    readonly issueStatus: (body: object) => Promise<unknown>;
    readonly gateRegister: (body: object) => Promise<unknown>;
    readonly waveRelease: (body: object) => Promise<unknown>;
    readonly phaseBackward: (body: object) => Promise<unknown>;
    readonly phaseRetry: (body: object) => Promise<unknown>;
    readonly signOff: (body: object) => Promise<unknown>;
  };
  const calls: ReadonlyArray<readonly [keyof WorkflowClient, string, object, string]> = [
    [
      "grant",
      "/legion/v1/grants",
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-209" },
      "grant.json",
    ],
    ["githubToken", "/legion/v1/gh-token", { grantId: "grant-208" }, "github-token.json"],
    ["gitCredential", "/legion/v1/git-credential", { grantId: "grant-208" }, "git-credential.json"],
    [
      "provisioningCredential",
      "/legion/v1/provisioning-credential",
      { grantId: "grant-208" },
      "provisioning-credential.json",
    ],
    [
      "handoffComplete",
      "/legion/v1/handoff/complete",
      { grantId: "grant-208", summary: "done", verdict: "", ready: false, commit: "abc123" },
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
    ["signOff", "/legion/v1/signoff", { grantId: "grant-208", issue: "LEGION-208" }, "signoff.json"],
  ];
  const { fetch, requests } = daemon((url) => {
    const call = calls.find(([, route]) => route === url.pathname);
    return Response.json(call === undefined ? { error: "no route" } : goFixture(call[3]));
  });
  const client = createLegionGoDaemonClient("http://daemon.test", fetch) as unknown as WorkflowClient;

  for (const [operation, route, body, fixture] of calls) {
    expect(client[operation], `${operation} is available`).toBeDefined();
    await expect(client[operation](body)).resolves.toEqual(goFixture(fixture));
    expect(requests.at(-1)).toEqual({
      method: "POST",
      url: `http://daemon.test${route}`,
      body,
    });
  }
});

test("a workflow refusal surfaces its stable code and message", async () => {
  const client = createLegionGoDaemonClient(
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

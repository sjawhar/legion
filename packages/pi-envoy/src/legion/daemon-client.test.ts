import { expect, test } from "bun:test";
import {
  createLegionDaemonClient,
  LegionDaemonApiError,
  LegionDaemonTransportError,
  SPAWN_WORKER_RETRY_DELAYS_MS,
} from "./daemon-client";

test("reads the Legion project from daemon state", async () => {
  const requests: { readonly method: string; readonly path: string }[] = [];
  const redactedState = {
    project: "omp",
    version: 21,
    issues: {},
    trees: {},
    admission: { cap: 2, active: [], queue: [] },
    gates: {},
    roles: {},
    controllerPendingNotices: 0,
    pendingStatusWrites: [],
    workerAdmission: { queue: [] },
  };
  const client = createLegionDaemonClient("http://daemon.test", (async (input, init) => {
    const url = new URL(input.toString());
    requests.push({ method: init?.method ?? "GET", path: url.pathname });
    return Response.json(redactedState);
  }) as typeof fetch);

  await expect(client.state()).resolves.toEqual(redactedState);
  expect(requests).toEqual([{ method: "GET", path: "/legion/v1/state" }]);
});

test("requests a provisioning credential with an architect capability", async () => {
  const requests: { readonly method: string; readonly path: string; readonly body: unknown }[] = [];
  const client = createLegionDaemonClient("http://daemon.test", (async (input, init) => {
    const url = new URL(input.toString());
    const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
    requests.push({ method: init?.method ?? "GET", path: url.pathname, body });
    return Response.json({ token: "installation-token" });
  }) as typeof fetch);

  await expect(
    client.provisioningCredential({
      issue: "acme/widgets#42",
      secret: "architect-capability",
      sessionId: "ses_architect",
      tree: "acme/widgets#1",
    })
  ).resolves.toEqual({ token: "installation-token" });
  expect(requests).toEqual([
    {
      method: "POST",
      path: "/legion/v1/provisioning-credential",
      body: {
        issue: "acme/widgets#42",
        secret: "architect-capability",
        sessionId: "ses_architect",
        tree: "acme/widgets#1",
      },
    },
  ]);
});

test("registers controller readiness with its session capability", async () => {
  const requests: { readonly method: string; readonly path: string; readonly body: unknown }[] = [];
  const client = createLegionDaemonClient("http://daemon.test", (async (input, init) => {
    const url = new URL(input.toString());
    const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
    requests.push({ method: init?.method ?? "GET", path: url.pathname, body });
    return Response.json({});
  }) as typeof fetch);

  await expect(
    client.controllerReady({ secret: "controller-capability", sessionId: "ses_interactive" })
  ).resolves.toBeUndefined();
  expect(requests).toEqual([
    {
      method: "POST",
      path: "/legion/v1/controller/ready",
      body: { secret: "controller-capability", sessionId: "ses_interactive" },
    },
  ]);
});

test("recovers an invalid Legion session secret once before retrying the original request", async () => {
  const requests: { readonly path: string; readonly body: Record<string, unknown> }[] = [];
  const recovered: string[] = [];
  const client = createLegionDaemonClient(
    "http://daemon.test",
    (async (input, init) => {
      const url = new URL(input.toString());
      const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
      requests.push({ path: url.pathname, body });
      if (url.pathname === "/legion/v1/worker-session") {
        return Response.json({
          tree: "acme/widgets#1",
          issue: "acme/widgets#42",
          role: "implementer",
          secret: "recovered-secret",
        });
      }
      if (body.secret === "stale-secret") {
        return Response.json({ error: "Invalid session secret" }, { status: 403 });
      }
      return Response.json({ grantId: "grant-1", expiresAt: "2026-08-25T00:00:00.000Z" });
    }) as typeof fetch,
    {
      recoveryToken: () => "spawn-capability",
      onRecovered: (sessionId, session) => recovered.push(`${sessionId}:${session.secret}`),
    }
  );

  await expect(
    client.grant({
      tree: "acme/widgets#1",
      issue: "acme/widgets#42",
      sessionId: "ses_worker",
      secret: "stale-secret",
    })
  ).resolves.toEqual({ grantId: "grant-1", expiresAt: "2026-08-25T00:00:00.000Z" });
  expect(requests).toEqual([
    {
      path: "/legion/v1/grants",
      body: {
        tree: "acme/widgets#1",
        issue: "acme/widgets#42",
        sessionId: "ses_worker",
        secret: "stale-secret",
      },
    },
    {
      path: "/legion/v1/worker-session",
      body: { sessionId: "ses_worker", recoveryToken: "spawn-capability" },
    },
    {
      path: "/legion/v1/grants",
      body: {
        tree: "acme/widgets#1",
        issue: "acme/widgets#42",
        sessionId: "ses_worker",
        secret: "recovered-secret",
      },
    },
  ]);
  expect(recovered).toEqual(["ses_worker:recovered-secret"]);
});

test("leaves a revoked Legion role loudly forbidden without retrying the original request", async () => {
  const requests: string[] = [];
  const client = createLegionDaemonClient(
    "http://daemon.test",
    (async (input) => {
      const path = new URL(input.toString()).pathname;
      requests.push(path);
      if (path === "/legion/v1/worker-session") {
        return Response.json(
          { error: "Worker session is not registered for this agent" },
          { status: 403 }
        );
      }
      return Response.json({ error: "Invalid session secret" }, { status: 403 });
    }) as typeof fetch,
    { recoveryToken: () => "revoked-capability" }
  );

  await expect(
    client.grant({
      tree: "acme/widgets#1",
      issue: "acme/widgets#42",
      sessionId: "ses_revoked",
      secret: "stale-secret",
    })
  ).rejects.toThrow(
    'POST /legion/v1/worker-session failed with 403: {"error":"Worker session is not registered for this agent"}'
  );
  expect(requests).toEqual(["/legion/v1/grants", "/legion/v1/worker-session"]);
});

/** The daemon right after a restart: no secret for the session; every `/worker-session` mints and
 * stores a fresh one at once (`setCapability` overwrites at mint time) and only its response can be
 * held; every capability-bearing route is refused unless it carries the stored secret. */
function forgetfulDaemon(
  options: {
    readonly holdRecoveries?: boolean;
    readonly recoveryFails?: boolean;
    readonly recoveredRole?: string;
  } = {}
) {
  const requests: { readonly path: string; readonly secret?: unknown }[] = [];
  const held: (() => void)[] = [];
  const firstRecoverySeen = Promise.withResolvers<void>();
  let current: string | undefined;
  let minted = 0;
  const fetchFn = (async (input, init) => {
    const path = new URL(input.toString()).pathname;
    const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
    requests.push({ path, secret: body.secret });
    if (path === "/legion/v1/worker-session") {
      minted += 1;
      firstRecoverySeen.resolve();
      if (options.recoveryFails) {
        return Response.json(
          { error: "Worker session is not bound to a daemon-issued recovery token" },
          { status: 403 }
        );
      }
      const secret = `recovered-${minted}`;
      current = secret;
      if (options.holdRecoveries) {
        const gate = Promise.withResolvers<void>();
        held.push(gate.resolve);
        await gate.promise;
      }
      return Response.json({
        tree: "acme/widgets#1",
        issue: "acme/widgets#1",
        role: options.recoveredRole ?? "architect",
        secret,
      });
    }
    if (current === undefined || body.secret !== current) {
      return Response.json({ error: "Invalid session secret" }, { status: 403 });
    }
    if (path === "/legion/v1/waves/release") return Response.json({ released: ["acme/widgets#2"] });
    return Response.json({});
  }) as typeof fetch;
  return {
    requests,
    held,
    fetchFn,
    firstRecoverySeen: firstRecoverySeen.promise,
    forget: () => {
      current = undefined;
    },
    recoveryCount: () =>
      requests.filter((request) => request.path === "/legion/v1/worker-session").length,
  };
}

const architectCall = { tree: "acme/widgets#1", sessionId: "ses_root", secret: "boot-secret" };

test("two requests refused together share one recovery and both retry with its secret", async () => {
  const daemon = forgetfulDaemon({ holdRecoveries: true });
  const recovered: string[] = [];
  const client = createLegionDaemonClient("http://daemon.test", daemon.fetchFn, {
    recoveryToken: () => "root-boot-token",
    onRecovered: (sessionId, session) => recovered.push(`${sessionId}:${session.secret}`),
  });

  const release = client.releaseWave({ ...architectCall, issues: ["acme/widgets#2"] });
  const escalate = client.escalate({
    ...architectCall,
    kind: "capacity",
    context: { reason: "x" },
  });
  await daemon.firstRecoverySeen;
  // One macrotask tick, never a wall-clock wait: the fake is pure promises, so every microtask the
  // two refusals queued has run and a second recovery request (the defect) is already visible.
  await new Promise<void>((resolve) => setImmediate(resolve));
  for (const resume of daemon.held) resume();

  expect(await Promise.all([release, escalate])).toEqual([
    { released: ["acme/widgets#2"] },
    undefined,
  ]);
  expect(daemon.recoveryCount()).toBe(1);
  expect(recovered).toEqual(["ses_root:recovered-1"]);
  expect(daemon.requests.map((request) => request.secret)).toEqual([
    "boot-secret",
    "boot-secret",
    undefined,
    "recovered-1",
    "recovered-1",
  ]);
});

test("a request refused after a recovery already replaced its secret retries with the newer secret without asking again", async () => {
  const daemon = forgetfulDaemon();
  const client = createLegionDaemonClient("http://daemon.test", daemon.fetchFn, {
    recoveryToken: () => "root-boot-token",
  });

  await client.releaseWave({ ...architectCall, issues: [] });
  // A caller that still holds the boot secret (a closure captured before the recovery).
  await client.releaseWave({ ...architectCall, issues: [] });

  expect(daemon.recoveryCount()).toBe(1);
  expect(daemon.requests.map((request) => request.secret)).toEqual([
    "boot-secret",
    undefined,
    "recovered-1",
    "boot-secret",
    "recovered-1",
  ]);
});

test("a failed recovery fails every waiting request and the next refusal starts a new recovery", async () => {
  const daemon = forgetfulDaemon({ recoveryFails: true });
  const client = createLegionDaemonClient("http://daemon.test", daemon.fetchFn, {
    recoveryToken: () => "root-boot-token",
  });

  const results = await Promise.allSettled([
    client.releaseWave({ ...architectCall, issues: [] }),
    client.releaseWave({ ...architectCall, issues: [] }),
  ]);

  expect(results.map((result) => result.status)).toEqual(["rejected", "rejected"]);
  expect(daemon.recoveryCount()).toBe(1);
  await expect(client.releaseWave({ ...architectCall, issues: [] })).rejects.toThrow(
    "POST /legion/v1/worker-session failed with 403"
  );
  expect(daemon.recoveryCount()).toBe(2);
});

test("a recovery onRecovered rejects caches nothing: the next refusal recovers again", async () => {
  const daemon = forgetfulDaemon({ recoveredRole: "tester" });
  const client = createLegionDaemonClient("http://daemon.test", daemon.fetchFn, {
    recoveryToken: () => "root-boot-token",
    onRecovered: (_sessionId, session) => {
      if (session.role !== "architect") {
        throw new Error("Daemon recovered a capability for a different Legion role");
      }
    },
  });

  await expect(client.releaseWave({ ...architectCall, issues: [] })).rejects.toThrow(
    "different Legion role"
  );
  await expect(client.releaseWave({ ...architectCall, issues: [] })).rejects.toThrow(
    "different Legion role"
  );
  expect(daemon.recoveryCount()).toBe(2);
});

test("a request still refused after retrying with the recovered secret is returned after exactly one recovery", async () => {
  const daemon = forgetfulDaemon();
  let forgetOnRetry = true;
  const client = createLegionDaemonClient(
    "http://daemon.test",
    (async (input, init) => {
      const body = JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>;
      // The daemon restarts again between the recovery and the retry.
      if (body.secret === "recovered-1" && forgetOnRetry) {
        forgetOnRetry = false;
        daemon.forget();
      }
      return daemon.fetchFn(input, init);
    }) as typeof fetch,
    { recoveryToken: () => "root-boot-token" }
  );

  await expect(client.releaseWave({ ...architectCall, issues: [] })).rejects.toThrow(
    "POST /legion/v1/waves/release failed with 403"
  );
  expect(daemon.requests.map((request) => request.path)).toEqual([
    "/legion/v1/waves/release",
    "/legion/v1/worker-session",
    "/legion/v1/waves/release",
  ]);
});

const spawnCall = {
  tree: "REPO-42",
  issue: "REPO-43",
  role: "tester" as const,
  task: "verify",
  sessionId: "ses",
  secret: "s",
  requestId: "4e0aca36-77b3-43bd-96cf-d58890ae64e4",
};
const connectionRefused = () =>
  Object.assign(new Error("Unable to connect. Is the computer able to access the url?"), {
    code: "ConnectionRefused",
  });
const queuedResponse = () =>
  Response.json({ status: "queued", roleToken: "legion-omp-REPO-43-tester" });

/** Consumes one scripted error or response per fetch and records requests and retry delays. */
function scriptedSpawnFetch(script: Array<Error | Response>) {
  const requests: { readonly path: string; readonly body: Record<string, unknown> }[] = [];
  const sleeps: number[] = [];
  const fetchFn = (async (input, init) => {
    const path = new URL(input.toString()).pathname;
    requests.push({
      path,
      body: JSON.parse(init?.body?.toString() ?? "{}") as Record<string, unknown>,
    });
    const next = script.shift();
    if (next === undefined) throw new Error("scripted fetch exhausted");
    if (next instanceof Error) throw next;
    return next;
  }) as typeof fetch;
  const sleep = async (ms: number) => {
    sleeps.push(ms);
  };
  return { requests, sleeps, fetchFn, sleep };
}

test("retries spawn_worker only when the fetch itself rejects, with the same requestId, and answers the attempt that reaches the daemon", async () => {
  const daemon = scriptedSpawnFetch([connectionRefused(), queuedResponse()]);
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon.fetchFn,
    undefined,
    daemon.sleep
  );

  await expect(client.spawnWorker(spawnCall)).resolves.toEqual({
    status: "queued",
    roleToken: "legion-omp-REPO-43-tester",
  });
  expect(daemon.requests.map((request) => request.path)).toEqual([
    "/legion/v1/worker/spawn",
    "/legion/v1/worker/spawn",
  ]);
  expect(daemon.requests.map((request) => request.body.requestId)).toEqual([
    spawnCall.requestId,
    spawnCall.requestId,
  ]);
  expect(daemon.sleeps).toEqual([SPAWN_WORKER_RETRY_DELAYS_MS[0]]);
});

test("surfaces the real cause, the attempts, and the request id after three transport rejections", async () => {
  const third = connectionRefused();
  const daemon = scriptedSpawnFetch([connectionRefused(), connectionRefused(), third]);
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon.fetchFn,
    undefined,
    daemon.sleep
  );

  const failure = await client.spawnWorker(spawnCall).then(
    () => undefined,
    (error: unknown) => error
  );

  expect(failure).toBeInstanceOf(LegionDaemonTransportError);
  if (!(failure instanceof LegionDaemonTransportError)) throw new Error("expected transport error");
  expect(failure.message).toContain("Unable to connect");
  expect(failure.message).toContain("3 attempts");
  expect(failure.message).toContain(spawnCall.requestId);
  expect(failure.message).toContain("may have received");
  expect(failure.message).toContain("legion state");
  expect(failure.attempts).toBe(3);
  expect(failure.cause).toBe(third);
  expect(daemon.requests).toHaveLength(3);
  expect(daemon.sleeps).toEqual([...SPAWN_WORKER_RETRY_DELAYS_MS]);
});

test("never retries a daemon HTTP error on spawn_worker", async () => {
  for (const status of [409, 500]) {
    const daemon = scriptedSpawnFetch([
      Response.json({ error: `daemon said ${status}` }, { status }),
    ]);
    const client = createLegionDaemonClient(
      "http://daemon.test",
      daemon.fetchFn,
      undefined,
      daemon.sleep
    );

    const failure = await client.spawnWorker(spawnCall).then(
      () => undefined,
      (error: unknown) => error
    );

    expect(failure).toBeInstanceOf(LegionDaemonApiError);
    if (!(failure instanceof LegionDaemonApiError)) throw new Error("expected API error");
    expect(failure.status).toBe(status);
    expect(daemon.requests).toHaveLength(1);
    expect(daemon.sleeps).toEqual([]);
  }
});

test("composes with the 403 Invalid session secret recovery under one request id and one retry budget", async () => {
  const daemon = scriptedSpawnFetch([
    connectionRefused(),
    Response.json({ error: "Invalid session secret" }, { status: 403 }),
    Response.json({ tree: "REPO-42", issue: "REPO-42", role: "architect", secret: "fresh" }),
    queuedResponse(),
  ]);
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon.fetchFn,
    { recoveryToken: () => "root-boot-token" },
    daemon.sleep
  );

  await expect(client.spawnWorker(spawnCall)).resolves.toEqual({
    status: "queued",
    roleToken: "legion-omp-REPO-43-tester",
  });
  expect(daemon.requests.map((request) => request.path)).toEqual([
    "/legion/v1/worker/spawn",
    "/legion/v1/worker/spawn",
    "/legion/v1/worker-session",
    "/legion/v1/worker/spawn",
  ]);
  const spawnRequests = daemon.requests.filter((r) => r.path === "/legion/v1/worker/spawn");
  expect(spawnRequests.map((request) => request.body.requestId)).toEqual([
    spawnCall.requestId,
    spawnCall.requestId,
    spawnCall.requestId,
  ]);
  expect(spawnRequests.at(-1)?.body.secret).toBe("fresh");
  expect(daemon.sleeps).toEqual([SPAWN_WORKER_RETRY_DELAYS_MS[0]]);
});

test("allows two rejected spawn fetches after a 403 recovery before the third succeeds", async () => {
  const daemon = scriptedSpawnFetch([
    Response.json({ error: "Invalid session secret" }, { status: 403 }),
    Response.json({ tree: "REPO-42", issue: "REPO-42", role: "architect", secret: "fresh" }),
    connectionRefused(),
    connectionRefused(),
    queuedResponse(),
  ]);
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon.fetchFn,
    { recoveryToken: () => "root-boot-token" },
    daemon.sleep
  );

  await expect(client.spawnWorker(spawnCall)).resolves.toEqual({
    status: "queued",
    roleToken: "legion-omp-REPO-43-tester",
  });
  const spawnRequests = daemon.requests.filter((request) => request.path === "/legion/v1/worker/spawn");
  expect(spawnRequests).toHaveLength(4);
  expect(spawnRequests.map((request) => request.body.requestId)).toEqual([
    spawnCall.requestId,
    spawnCall.requestId,
    spawnCall.requestId,
    spawnCall.requestId,
  ]);
  expect(spawnRequests.at(-1)?.body.secret).toBe("fresh");
  expect(daemon.sleeps).toEqual([...SPAWN_WORKER_RETRY_DELAYS_MS]);
});

test("reports three rejected spawn fetches after a 403 recovery", async () => {
  const third = connectionRefused();
  const daemon = scriptedSpawnFetch([
    Response.json({ error: "Invalid session secret" }, { status: 403 }),
    Response.json({ tree: "REPO-42", issue: "REPO-42", role: "architect", secret: "fresh" }),
    connectionRefused(),
    connectionRefused(),
    third,
  ]);
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon.fetchFn,
    { recoveryToken: () => "root-boot-token" },
    daemon.sleep
  );

  const failure = await client.spawnWorker(spawnCall).then(
    () => undefined,
    (error: unknown) => error
  );

  expect(failure).toBeInstanceOf(LegionDaemonTransportError);
  if (!(failure instanceof LegionDaemonTransportError)) throw new Error("expected transport error");
  expect(failure.message).toContain("3 attempts");
  expect(failure.attempts).toBe(3);
  expect(failure.cause).toBe(third);
  expect(daemon.requests).toHaveLength(5);
  expect(daemon.sleeps).toEqual([...SPAWN_WORKER_RETRY_DELAYS_MS]);
});

test("labels an incomplete spawn response with request-id guidance", async () => {
  const bodyReadError = new Error("connection closed while reading the response");
  const daemon = scriptedSpawnFetch([
    new Response(
      new ReadableStream({
        start(controller) {
          controller.error(bodyReadError);
        },
      })
    ),
  ]);
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon.fetchFn,
    undefined,
    daemon.sleep
  );

  const failure = await client.spawnWorker(spawnCall).then(
    () => undefined,
    (error: unknown) => error
  );

  expect(failure).toBeInstanceOf(Error);
  if (!(failure instanceof Error)) throw new Error("expected response read error");
  expect(failure.message).toContain(spawnCall.requestId);
  expect(failure.message).toContain("may have received");
  expect(failure.message).toContain("legion state");
  expect(failure.cause).toBe(bodyReadError);
  expect(daemon.requests).toHaveLength(1);
  expect(daemon.sleeps).toEqual([]);
});

import { expect, test } from "bun:test";
import { createLegionDaemonClient } from "./daemon-client";

test("reads the Legion project from daemon state", async () => {
  const requests: { readonly method: string; readonly path: string }[] = [];
  const client = createLegionDaemonClient(
    "http://daemon.test",
    (async (input, init) => {
      const url = new URL(input.toString());
      requests.push({ method: init?.method ?? "GET", path: url.pathname });
      return Response.json({ project: "omp" });
    }) as typeof fetch
  );

  await expect(client.state()).resolves.toEqual({ project: "omp" });
  expect(requests).toEqual([{ method: "GET", path: "/legion/v1/state" }]);
});

test("requests a provisioning credential with an architect capability", async () => {
  const requests: { readonly method: string; readonly path: string; readonly body: unknown }[] = [];
  const client = createLegionDaemonClient(
    "http://daemon.test",
    (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ method: init?.method ?? "GET", path: url.pathname, body });
      return Response.json({ token: "installation-token" });
    }) as typeof fetch
  );

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
  const client = createLegionDaemonClient(
    "http://daemon.test",
    (async (input, init) => {
      const url = new URL(input.toString());
      const body = init?.body == null ? undefined : JSON.parse(init.body.toString());
      requests.push({ method: init?.method ?? "GET", path: url.pathname, body });
      return Response.json({});
    }) as typeof fetch
  );

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

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

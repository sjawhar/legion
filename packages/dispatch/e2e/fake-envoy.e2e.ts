import { spawn } from "node:child_process";
import { once } from "node:events";
import { request as httpRequest } from "node:http";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { expect, test } from "@playwright/test";

import type { FakeInterest, FakeLegionAgent, FakeSession } from "./agents";
import { withoutHarnessPort } from "./harness-ports";

// The fake Envoy is the harness's own fixture, so these rows start a second one on a port of its
// own rather than reseeding the listener every other row in the run shares: with the fake Envoy
// port unset under every name this lane carries it under, the second fake binds a port the OS
// chooses, and the row reads it from the fake's listening line.
const fakeEnvoyModule = fileURLToPath(new URL("./fake-envoy.ts", import.meta.url));

/** A fake Envoy of the row's own, listening, and the way to stop it. */
async function startFakeEnvoy(): Promise<{
  origin: string;
  port: number;
  stop: () => Promise<void>;
}> {
  const env = withoutHarnessPort("fakeEnvoy");
  const fake = spawn("bun", [fakeEnvoyModule], { env, stdio: ["ignore", "pipe", "inherit"] });
  const exited = once(fake, "exit");
  const stop = async () => {
    fake.kill();
    await exited;
  };
  try {
    const ready = Promise.withResolvers<number>();
    let output = "";
    fake.stdout.setEncoding("utf8");
    fake.stdout.on("data", (chunk: string) => {
      output += chunk;
      const listening = /fake envoy listener on 127\.0\.0\.1:(\d+)\n/.exec(output);
      if (listening !== null) ready.resolve(Number(listening[1]));
    });
    fake.once("exit", (code) => {
      ready.reject(new Error(`the fake Envoy exited ${code} before it listened: ${output}`));
    });
    fake.once("error", ready.reject);
    const port = await ready.promise;
    return { origin: `http://127.0.0.1:${port}`, port, stop };
  } catch (error) {
    await stop();
    throw error;
  }
}

test("an interests seed whose body arrives during a session reseed is kept", async () => {
  test.skip(test.info().project.name !== "chromium", "the fixture has no browser to vary");
  const { origin, port, stop } = await startFakeEnvoy();
  try {
    // The interests PUT's headers go first and its body last, with a whole session reseed in
    // between: `conversation.e2e.ts` and `inbox.e2e.ts` send the two seeds at once, and a client
    // that splits a request's headers from its body puts the reseed inside the handler's await.
    const interests: FakeInterest[] = [{ session_id: "e2e-interest", topics: ["dispatch.CORE"] }];
    const body = JSON.stringify(interests);
    const answered = Promise.withResolvers<number>();
    const interestsPut = httpRequest(
      {
        headers: { "Content-Length": Buffer.byteLength(body), "Content-Type": "application/json" },
        host: "127.0.0.1",
        method: "PUT",
        path: "/__fixture/interests",
        port,
      },
      (response) => {
        response.resume();
        response.on("end", () => answered.resolve(response.statusCode ?? 0));
      }
    ).on("error", answered.reject);
    interestsPut.flushHeaders();
    // Long enough for the fixture to have entered the handler and be waiting on the body. A
    // reseed that lands before the handler starts cannot show the loss, so too short a wait
    // passes a broken fixture, never fails a sound one.
    await sleep(100);
    const sessions: FakeSession[] = [{ session_id: "e2e-reseeded", title: "Reseeded" }];
    const reseed = await fetch(`${origin}/__fixture/sessions`, {
      body: JSON.stringify(sessions),
      headers: { "Content-Type": "application/json" },
      method: "PUT",
    });
    expect(reseed.status).toBe(200);
    interestsPut.end(body);
    expect(await answered.promise).toBe(200);

    const stored = await fetch(`${origin}/v1/interests/`);
    expect(await stored.json()).toEqual(interests);
  } finally {
    await stop();
  }
});

// The registry reads answer from the seeded entries as the listener's do: an issue's sessions by
// role in workflow order and each role token's newest first, `replaced_by`/`replaced_at` from the
// next session of the token to start, and `live` from the session's own registration inside the
// live window (`sessionIsLive` through `session.StateOf`), never from this listener's session
// cache - so a session the cache holds whose registration is older than the window reads
// `live: false` while it is not gone. A session reseed keeps the entries, as the real bucket
// outlives a session ending.
test("the Legion agent registry reads derive live and replacement, and outlive a session reseed", async () => {
  test.skip(test.info().project.name !== "chromium", "the fixture has no browser to vary");
  const { origin, stop } = await startFakeEnvoy();
  const put = (path: string, body: unknown) =>
    fetch(`${origin}${path}`, {
      body: JSON.stringify(body),
      headers: { "Content-Type": "application/json" },
      method: "PUT",
    });
  const entry = (
    session: string,
    issue: string,
    role: FakeLegionAgent["role"],
    startedAt: string
  ): FakeLegionAgent => ({
    harness: "omp",
    issue,
    legion_project: "e2elegion",
    role,
    role_token: `legion-e2elegion-${issue.toLowerCase()}-${role}`,
    session_id: session,
    started_at: startedAt,
    title: `Legion ${role} · ${issue}`,
  });
  try {
    const seeded = [
      entry("new-reviewer", "E2E-1", "reviewer", "2026-10-26T02:00:00Z"),
      entry("old-reviewer", "E2E-1", "reviewer", "2026-10-26T01:00:00Z"),
      entry("implementer", "E2E-1", "implementer", "2026-10-26T00:00:00Z"),
      entry("stale-tester", "E2E-1", "tester", "2026-10-26T00:30:00Z"),
      entry("elsewhere", "E2E-2", "tester", "2026-10-26T00:00:00Z"),
    ];
    expect((await put("/__fixture/legion-agents", seeded)).status).toBe(200);
    // The reseed puts both sessions in the listener's cache; only the registration decides `live`,
    // and the tester's is an hour old.
    const sessions: FakeSession[] = [
      { session_id: "new-reviewer", title: "Legion reviewer" },
      {
        last_seen: Date.now() - 60 * 60 * 1000,
        session_id: "stale-tester",
        title: "Legion tester",
      },
    ];
    expect((await put("/__fixture/sessions", sessions)).status).toBe(200);

    const issue = await fetch(`${origin}/v1/legion/agents?issue=E2E-1`);
    expect(issue.status).toBe(200);
    expect(await issue.json()).toMatchObject({
      entries: [
        { live: false, replaced_at: null, replaced_by: null, session_id: "implementer" },
        { live: false, replaced_at: null, replaced_by: null, session_id: "stale-tester" },
        { live: true, replaced_at: null, replaced_by: null, session_id: "new-reviewer" },
        {
          live: false,
          replaced_at: "2026-10-26T02:00:00Z",
          replaced_by: "new-reviewer",
          session_id: "old-reviewer",
        },
      ],
    });

    const role = await fetch(`${origin}/v1/legion/agents/roles/legion-e2elegion-e2e-1-reviewer`);
    expect(await role.json()).toMatchObject({
      entries: [{ session_id: "new-reviewer" }, { session_id: "old-reviewer" }],
    });
    const none = await fetch(`${origin}/v1/legion/agents/roles/legion-e2elegion-e2e-1-merger`);
    expect(await none.json()).toEqual({ entries: [] });

    const old = await fetch(`${origin}/v1/legion/agents/sessions/old-reviewer`);
    expect(old.status).toBe(200);
    expect(await old.json()).toMatchObject({
      issue: "E2E-1",
      live: false,
      replaced_by: "new-reviewer",
      role_token: "legion-e2elegion-e2e-1-reviewer",
    });
    const unknown = await fetch(`${origin}/v1/legion/agents/sessions/nobody`);
    expect(unknown.status).toBe(404);
    expect(await unknown.json()).toEqual({ error: "unknown legion agent nobody" });
  } finally {
    await stop();
  }
});

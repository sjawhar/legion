import { expect, mock, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { isolateEnvoyConfig } from "./isolated-config";

// Each test's scratch HOME, holding the empty envoy.json ENVOY_CONFIG_FILE names.
const configDirectory = isolateEnvoyConfig();

const fixture = path.join(import.meta.dir, "serve-signal-fixture.ts");

/** A listener that answers every registration and records what reached it. */
function listenerRecording(calls: string[]) {
  return Bun.serve({
    port: 0,
    fetch: (request) => {
      const url = new URL(request.url);
      calls.push(`${request.method} ${url.pathname}`);
      // The plugin reads nothing out of a registration's answer, only that it parses.
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        return Response.json({
          session_id: "ses_registered",
          machine_id: "test",
          dir: "/tmp",
          topics: [],
        });
      }
      return new Response(null, { status: 200 });
    },
  });
}

// OpenCode deleting a session used to leave its registration for the listener to expire: for the
// whole live window peers read it as live, and for the gone horizon after it as unresponsive, so
// deliveries went on being attempted at this serve's port for a session that no longer exists
// (dispatch://INFRA-392).
test("session.deleted deregisters the session with the listener", async () => {
  const calls: string[] = [];
  const listener = listenerRecording(calls);
  const originalEnvoyUrl = process.env.ENVOY_URL;
  const originalError = console.error;
  console.error = mock(() => {});
  process.env.ENVOY_URL = `http://127.0.0.1:${listener.port}`;
  let dispose: (() => void) | undefined;

  try {
    const pluginModule = await import("../server");
    const hooks = await pluginModule.default({
      serverUrl: new URL("http://127.0.0.1:13381/"),
    } as never);
    dispose = hooks.dispose;
    await hooks.event({
      event: {
        type: "session.status",
        properties: { sessionID: "ses_deleted", status: { type: "busy" } },
      },
    });

    await hooks.event({
      event: { type: "session.deleted", properties: { sessionID: "ses_deleted" } },
    });

    expect(calls).toContain("DELETE /v1/sessions/ses_deleted");
  } finally {
    dispose?.();
    console.error = originalError;
    listener.stop(true);
    if (originalEnvoyUrl === undefined) delete process.env.ENVOY_URL;
    else process.env.ENVOY_URL = originalEnvoyUrl;
  }
});

// A serve is stopped by a signal, and the `exit` handler it had could not wait for a listener
// call: every session it was running stayed registered, and its peers kept sending to a process
// that had gone.
test("a SIGTERM to the serve deregisters every session it was running", async () => {
  const calls: string[] = [];
  const listener = listenerRecording(calls);
  const home = mkdtempSync(path.join(os.tmpdir(), "envoy-plugin-signal-"));
  writeFileSync(path.join(home, "envoy.json"), "{}\n");
  const serve = Bun.spawn(["bun", fixture, "ses_one", "ses_two"], {
    cwd: configDirectory(),
    env: {
      PATH: process.env.PATH ?? "",
      HOME: home,
      ENVOY_CONFIG_FILE: path.join(home, "envoy.json"),
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      ENVOY_HEARTBEAT_MS: "60000",
    },
    stdout: "pipe",
    stderr: "pipe",
  });

  try {
    const reader = serve.stdout.getReader();
    const decoder = new TextDecoder();
    let said = "";
    while (!said.includes("ready")) {
      const { done, value } = await reader.read();
      if (done) throw new Error(`the serve fixture ended before it was ready: ${said}`);
      said += decoder.decode(value);
    }
    reader.releaseLock();

    serve.kill("SIGTERM");
    await serve.exited;

    expect(calls).toContain("DELETE /v1/sessions/ses_one");
    expect(calls).toContain("DELETE /v1/sessions/ses_two");
  } finally {
    serve.kill("SIGKILL");
    listener.stop(true);
    rmSync(home, { recursive: true, force: true });
  }
}, 20_000);

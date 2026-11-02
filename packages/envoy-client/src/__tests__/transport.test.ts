import { describe, expect, test } from "bun:test";
import { MINTED_DEDUPE_KEY_PATTERN } from "@legion/contracts";
import { machineID } from "../machine";
import {
  createEnvoyClient,
  type DeliveryAnswer,
  EnvoyApiError,
  EnvoySendUnconfirmedError,
  newClientInstanceID,
} from "../transport";

type RecordedFetch = {
  readonly fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
  readonly requests: Request[];
};

function recordFetch(responses: readonly Response[]): RecordedFetch {
  const requests: Request[] = [];
  let nextResponse = 0;

  return {
    fetch: async (input, init) => {
      requests.push(new Request(input, init));
      const response = responses[nextResponse];
      nextResponse += 1;
      if (!response) throw new Error("test fetch received an unexpected request");
      return response;
    },
    requests,
  };
}

const PLUGIN = { name: "@sjawhar/pi-envoy", version: "11.0.0" };

function jsonResponse(body: unknown): Response {
  return Response.json(body);
}

describe("EnvoyClient", () => {
  test("posts a subscription using the listener request shape", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.agent.ses_sender"],
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://127.0.0.1:39121/", fetch: recorded.fetch });

    const interest = await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 13381,
      title: "Envoy QA",
      driving: true,
      clientInstanceID: "0f8c2b1a4d6e7f90",
      plugin: PLUGIN,
    });

    expect(interest).toEqual({
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      updated_at: undefined,
      delivery: { mode: "core", reason: "not_answered" },
    });
    expect(recorded.requests).toHaveLength(1);
    expect(recorded.requests[0]?.method).toBe("POST");
    expect(recorded.requests[0]?.url).toBe("http://127.0.0.1:39121/v1/interests/subscribe");
    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_sender",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 13381,
      title: "Envoy QA",
      driving: true,
      client_instance_id: "0f8c2b1a4d6e7f90",
      host: machineID(),
      plugin: "@sjawhar/pi-envoy",
      plugin_version: "11.0.0",
    });
  });

  test("a subscription without a client instance id carries no client_instance_id", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.agent.ses_sender"],
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 13381,
      title: "Envoy QA",
      driving: true,
      plugin: PLUGIN,
    });

    const body = (await recorded.requests[0]?.json()) as Record<string, unknown>;
    // The key is absent, not undefined, so an older listener sees an unchanged body.
    expect(Object.hasOwn(body, "client_instance_id")).toBe(false);
    expect(body).toEqual({
      session_id: "ses_sender",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 13381,
      title: "Envoy QA",
      driving: true,
      host: machineID(),
      plugin: "@sjawhar/pi-envoy",
      plugin_version: "11.0.0",
    });
  });

  test("asks for consumer delivery and reads each delivery answer", async () => {
    const interest = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
    };
    const consumer: DeliveryAnswer = {
      mode: "consumer",
      subject: "notifications.envoy.deliver.ses_sender",
      stream_id: "stream-1",
      gap: false,
    };
    const recorded = recordFetch([
      jsonResponse({ ...interest, delivery: consumer }),
      jsonResponse({ ...interest, delivery: { mode: "core", reason: "no_grant" } }),
      jsonResponse(interest),
      jsonResponse({ ...interest, delivery: { mode: "push" } }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
    const register = () =>
      client.subscribe({
        sessionID: "ses_sender",
        directory: "/work",
        topics: ["notifications.agent.ses_sender"],
        port: 0,
        title: "",
        driving: false,
        selfSubscribed: true,
        delivery: { consumer: true, epoch: "epoch-1", resume: { stream_id: "stream-1", seq: 41 } },
        plugin: PLUGIN,
      });

    expect((await register()).delivery).toEqual(consumer);
    expect((await register()).delivery).toEqual({ mode: "core", reason: "no_grant" });
    // A listener of an earlier release answers no `delivery`: core.
    expect((await register()).delivery).toEqual({ mode: "core", reason: "not_answered" });
    // A mode this build does not know does not fail the registration: core.
    expect((await register()).delivery).toEqual({
      mode: "core",
      reason: "unreadable_answer",
    });
    // The registration carries the plugin beside the delivery request.
    expect(await recorded.requests[0]?.json()).toMatchObject({
      self_subscribed: true,
      delivery: { consumer: true, epoch: "epoch-1", resume: { stream_id: "stream-1", seq: 41 } },
      plugin: "@sjawhar/pi-envoy",
      plugin_version: "11.0.0",
    });
  });

  test("expands a concrete trailing wildcard for subscribe and unsubscribe", async () => {
    const wildcard = "notifications.github.example-org.example-repo.pr.42.>";
    const base = "notifications.github.example-org.example-repo.pr.42";
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: [base, wildcard],
      }),
      new Response("ok"),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: [wildcard],
      port: 0,
      title: "",
      driving: true,
      plugin: PLUGIN,
    });
    await client.unsubscribe({ sessionID: "ses_sender", topics: [wildcard] });

    expect(await recorded.requests[0]?.json()).toMatchObject({ topics: [base, wildcard] });
    expect(await recorded.requests[1]?.json()).toEqual({
      session_id: "ses_sender",
      topics: [base, wildcard],
    });
  });

  test("does not expand a trailing wildcard whose base contains a wildcard", async () => {
    const wildcard = "notifications.github.example-org.example-repo.pr.*.>";
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: [wildcard],
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: [wildcard],
      port: 0,
      title: "",
      driving: true,
      plugin: PLUGIN,
    });

    expect(await recorded.requests[0]?.json()).toMatchObject({ topics: [wildcard] });
  });
  test("rejects a wildcard whose concrete base has empty segments", async () => {
    const recorded = recordFetch([]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    for (const topic of ["a..b.>", "a.>"]) {
      await expect(
        client.subscribe({
          sessionID: "ses_sender",
          directory: "/work",
          topics: [topic],
          port: 0,
          title: "",
          driving: true,
          plugin: PLUGIN,
        })
      ).rejects.toThrow("concrete base");
    }

    expect(recorded.requests).toEqual([]);
  });

  test("posts an unsubscribe request with an empty topic list", async () => {
    const recorded = recordFetch([new Response("ok")]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.unsubscribe({ sessionID: "ses_sender", topics: [] });

    expect(recorded.requests[0]?.url).toBe("http://listener/v1/interests/unsubscribe");
    expect(await recorded.requests[0]?.json()).toEqual({ session_id: "ses_sender", topics: [] });
  });

  test("gets one session interest using the listener response shape", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.agent.ses_sender"],
        updated_at: 42,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const interest = await client.getInterest("ses_sender");

    expect(interest).toEqual({
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      updated_at: 42,
    });
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/interests/ses_sender");
  });

  test("sends a failed request once more by default, and only once with retry false", async () => {
    const down = () => new Response("down", { status: 503 });
    for (const [retry, sent] of [
      [undefined, 2],
      [false, 1],
    ] as const) {
      const recorded = recordFetch([down(), down()]);
      const client = createEnvoyClient({
        baseUrl: "http://listener",
        fetch: recorded.fetch,
        ...(retry === undefined ? {} : { retry }),
      });
      await expect(client.getInterest("ses_sender")).rejects.toThrow("failed with 503");
      expect([retry, recorded.requests.length]).toEqual([retry, sent]);
    }
  });

  test("sends a direct message and parses the listener envelope", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-1",
        source: "agent",
        source_event_id: "agent.ses_sender.event-1",
        source_session: "ses_sender",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.event-1",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-1",
        recipient: "ses_target",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hello",
    });

    expect(result.envelope.topic).toBe("notifications.agent.ses_target");
    expect(result.recipient).toBe("ses_target");
    expect(result.confirmed).toBe(true);

    expect(await recorded.requests[0]?.json()).toMatchObject({
      source: "agent",
      source_session: "ses_sender",
      target_session: "ses_target",
      message: "hello",
      idempotency_key: expect.any(String),
    });
  });

  test("returns the requested recipient when a legacy listener omits it", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-legacy",
        source: "agent",
        source_event_id: "agent.ses_sender.event-legacy",
        source_session: "ses_sender",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.event-legacy",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-legacy",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hello",
    });

    expect(result).toMatchObject({
      envelope: { event_id: "event-legacy" },
      recipient: "ses_target",
      confirmed: false,
    });
  });

  test("sends a human direct message without a source session", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-human",
        source: "human",
        source_event_id: "human.event-1",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.human-event-1",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-human",
        recipient: "ses_target",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.send({
      source: "human",
      targetSessionID: "ses_target",
      message: "hello",
    });

    expect(await recorded.requests[0]?.json()).toMatchObject({
      source: "human",
      target_session: "ses_target",
      message: "hello",
      idempotency_key: expect.any(String),
    });
  });

  test("omits an empty source session from a send and a publish", async () => {
    // A caller with no session of its own (a task subagent in a host that never registered
    // one) must send no `source_session` at all: the listener's own JSON drops an empty one on
    // the way out, leaving the recipient a message with no sender and no reply hint.
    const envelope = {
      event_id: "event-sourceless",
      source: "agent",
      source_event_id: "agent.event-sourceless",
      topic: "notifications.agent.ses_target",
      dedupe_key: "agent.ses_target.event-sourceless",
      issued_at: 1,
      payload_summary: "hello",
      trace_id: "trace-sourceless",
    };
    const recorded = recordFetch([jsonResponse(envelope), jsonResponse(envelope)]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.send({ sourceSessionID: "", targetSessionID: "ses_target", message: "hello" });
    await client.publish({
      sourceSessionID: "",
      topic: "notifications.role.controller",
      message: "broadcast",
    });

    expect(await recorded.requests[0]?.json()).not.toHaveProperty("source_session");
    expect(await recorded.requests[1]?.json()).not.toHaveProperty("source_session");
  });

  test("publishes a broadcast using the listener request shape", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-2",
        source: "agent",
        source_event_id: "agent.ses_sender.event-2",
        source_session: "ses_sender",
        topic: "notifications.role.controller",
        dedupe_key: "agent.controller.event-2",
        issued_at: 2,
        payload_summary: "broadcast",
        trace_id: "trace-2",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.publish({
      sourceSessionID: "ses_sender",
      topic: "notifications.role.controller",
      message: "broadcast",
      payload: `{"kind":"role-event"}`,
    });
    expect(result).toMatchObject({ envelope: { event_id: "event-2" } });
    expect(result.holder).toBeUndefined();

    expect(await recorded.requests[0]?.json()).toMatchObject({
      source: "agent",
      source_session: "ses_sender",
      topic: "notifications.role.controller",
      message: "broadcast",
      payload: `{"kind":"role-event"}`,
      idempotency_key: expect.any(String),
    });
  });

  test("deletes a session registration", async () => {
    const recorded = recordFetch([new Response(null, { status: 200 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.unregisterSession({ sessionID: "ses_closed" });

    expect(recorded.requests).toHaveLength(1);
    expect(recorded.requests[0]?.method).toBe("DELETE");
    // No client instance id, no query string: the request an older client sent.
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/sessions/ses_closed");
  });

  test("a deregistration with a client instance id sends it as an encoded query parameter", async () => {
    const recorded = recordFetch([
      Response.json({ deleted: true }, { status: 200 }),
      Response.json(
        { deleted: false, reason: "the registration belongs to another client instance" },
        { status: 200 }
      ),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.unregisterSession({ sessionID: "ses_closed", clientInstanceID: "0f8c/2b 1a" });
    // A refusal is a 200 whose body says the entry stayed; no caller acts on it.
    await client.unregisterSession({ sessionID: "ses_closed", clientInstanceID: "0f8c2b1a" });

    expect(recorded.requests[0]?.method).toBe("DELETE");
    expect(recorded.requests[0]?.url).toBe(
      "http://listener/v1/sessions/ses_closed?client_instance_id=0f8c%2F2b%201a"
    );
    expect(recorded.requests[1]?.url).toBe(
      "http://listener/v1/sessions/ses_closed?client_instance_id=0f8c2b1a"
    );
  });

  test("sets a role and returns the listener interest response", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.role.controller"],
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.setRole({ sessionID: "ses_sender", role: "controller" });

    expect(result).toEqual({
      claimed: true,
      interest: expect.objectContaining({ topics: ["notifications.role.controller"] }),
    });
    // A hard claim sends no `soft` field, so an older listener sees an
    // unchanged request.
    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_sender",
      role: "controller",
    });
  });

  test("a soft claim sends soft:true and maps the listener's 409 to a not-claimed result", async () => {
    const recorded = recordFetch([
      Response.json(
        { error: "role controller is held by ses_live", role: "controller", holder: "ses_live" },
        { status: 409 }
      ),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.setRole({
      sessionID: "ses_resumer",
      role: "controller",
      soft: true,
    });

    expect(result).toEqual({ claimed: false, holder: "ses_live" });
    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_resumer",
      role: "controller",
      soft: true,
    });
  });

  test("previous_session_id is sent with a soft claim and dropped from a hard one", async () => {
    const interest = { session_id: "ses_child", machine_id: "host-a", dir: "/work", topics: [] };
    const recorded = recordFetch([jsonResponse(interest), jsonResponse(interest)]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.setRole({
      sessionID: "ses_child",
      role: "controller",
      soft: true,
      previousSessionID: "ses_parent",
    });
    await client.setRole({
      sessionID: "ses_child",
      role: "controller",
      previousSessionID: "ses_parent",
    });

    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_child",
      role: "controller",
      soft: true,
      previous_session_id: "ses_parent",
    });
    // A hard claim is last-claim-wins already; the predecessor is meaningless
    // and stays off the wire.
    expect(await recorded.requests[1]?.json()).toEqual({
      session_id: "ses_child",
      role: "controller",
    });
  });

  test("a 409 without the held-role shape stays a typed API error", async () => {
    const recorded = recordFetch([Response.json({ error: "duplicate" }, { status: 409 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await expect(
      client.setRole({ sessionID: "ses_x", role: "controller", soft: true })
    ).rejects.toBeInstanceOf(EnvoyApiError);
  });

  test("a 409 whose body does not arrive stays a typed API error, sent once", async () => {
    let calls = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        calls += 1;
        const body = new ReadableStream({
          pull(controller) {
            controller.error(new TypeError("body lost"));
          },
        });
        return new Response(body, { status: 409 });
      },
    });

    await expect(
      client.setRole({ sessionID: "ses_x", role: "controller", soft: true })
    ).rejects.toBeInstanceOf(EnvoyApiError);
    expect(calls).toBe(1);
  });

  test("lists listener sessions and retains their registration metadata, plugin or none", async () => {
    const unversioned = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      port: 13381,
      title: "Envoy QA",
      topics: ["notifications.agent.ses_sender"],
      self_subscribed: false,
    };
    const versioned = {
      ...unversioned,
      session_id: "ses_versioned",
      topics: ["notifications.agent.ses_versioned"],
      plugin: "@sjawhar/claude-envoy",
      plugin_version: "afe10b2f3e1b",
    };
    const recorded = recordFetch([jsonResponse([unversioned, versioned])]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    expect(sessions).toEqual([unversioned, versioned]);
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/sessions");
  });

  test("normalizes non-success responses into a typed API error", async () => {
    const recorded = recordFetch([new Response("invalid role", { status: 400 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const request = client.setRole({ sessionID: "ses_sender", role: "invalid role" });

    await expect(request).rejects.toEqual(
      new EnvoyApiError({
        method: "POST",
        url: "http://listener/v1/roles/set",
        status: 400,
        responseBody: "invalid role",
      })
    );
  });

  test("retries one 503 response and keeps the direct-send recipient", async () => {
    const recorded = recordFetch([
      Response.json({ error: "listener is starting" }, { status: 503 }),
      jsonResponse({
        event_id: "event-retried",
        source: "agent",
        source_event_id: "agent.sender.event-retried",
        source_session: "01a00000-0000-7000-0000-000000000001",
        topic: "notifications.agent.01a01111-2222-7333-4444-555555555555",
        dedupe_key: "event-retried",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-retried",
        recipient: "01a01111-2222-7333-4444-555555555555",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.send({
      sourceSessionID: "01a00000-0000-7000-0000-000000000001",
      targetSessionID: "01a01111-2222-7333-4444-555555555555",
      message: "hello",
    });

    expect(recorded.requests).toHaveLength(2);
    expect(result).toMatchObject({
      recipient: "01a01111-2222-7333-4444-555555555555",
      envelope: { event_id: "event-retried" },
    });
  });
  test("retries a delivered direct message with one generated idempotency key", async () => {
    const idempotencyKeys: string[] = [];
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async (_input, init) => {
        const body = JSON.parse(String(init?.body)) as { readonly idempotency_key?: string };
        if (typeof body.idempotency_key !== "string") {
          return new Response("idempotency key required", { status: 400 });
        }
        idempotencyKeys.push(body.idempotency_key);
        if (idempotencyKeys.length === 1) {
          throw new TypeError("response lost after delivery");
        }
        if (body.idempotency_key !== idempotencyKeys[0]) {
          return new Response("duplicate message", { status: 409 });
        }
        return jsonResponse({
          event_id: "event-delivered",
          source: "agent",
          source_event_id: "agent.sender.event-delivered",
          source_session: "ses_sender",
          topic: "notifications.agent.ses_target",
          dedupe_key: "event-delivered",
          issued_at: 1,
          payload_summary: "hello",
          trace_id: "trace-delivered",
          recipient: "ses_target",
        });
      },
    });

    await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hello",
    });

    expect(idempotencyKeys).toHaveLength(2);
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);
  });

  test("a send no attempt of which is answered is not confirmed delivered, and names the key a resend reuses", async () => {
    const idempotencyKeys: string[] = [];
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async (_input, init) => {
        const body = JSON.parse(String(init?.body)) as { readonly idempotency_key: string };
        idempotencyKeys.push(body.idempotency_key);
        // What AbortSignal.timeout raises when the listener holds the request past the deadline.
        throw new DOMException("The operation timed out.", "TimeoutError");
      },
    });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(idempotencyKeys).toHaveLength(2);
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);
    const key = idempotencyKeys[0] as string;
    // The listener's test of a dedupe key a repeat is recognised by, so a resend under the key the
    // error names cannot deliver the message twice.
    expect(`agent.ses_target.${key}`).toMatch(new RegExp(MINTED_DEDUPE_KEY_PATTERN));
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    expect(error).toMatchObject({
      targetSessionID: "ses_target",
      idempotencyKey: key,
      attempts: 2,
      resendSafe: true,
    });
    expect((error as Error).message).toBe(
      "The send to ses_target is not confirmed delivered: the listener did not answer " +
        "(The operation timed out.) on the last of 2 attempts. It may or may not have arrived. " +
        `Sending it again with idempotency_key ${key} within 72 hours is the safe resend: ` +
        "Envoy drops it as a repeat if the first copy arrived (a recipient that restarted in " +
        "between can rarely see it twice)."
    );
  });

  test("a send refused after an unanswered attempt stays unconfirmed: that attempt may have stored it", async () => {
    let calls = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        calls += 1;
        if (calls === 1) throw new DOMException("The operation timed out.", "TimeoutError");
        return Response.json({ error: "no live session ses_target" }, { status: 404 });
      },
    });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(calls).toBe(2);
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    expect((error as EnvoySendUnconfirmedError).message).toStartWith(
      "The send to ses_target is not confirmed delivered: the listener answered 404 " +
        "(no live session ses_target) on the last of 2 attempts. It may or may not have arrived."
    );
  });

  test("a send whose 2xx answer does not parse is not confirmed delivered", async () => {
    const recorded = recordFetch([new Response("{", { status: 200 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    expect((error as EnvoySendUnconfirmedError).last).toBeInstanceOf(SyntaxError);
  });

  test("a send under a key no repeat is recognised by does not call a resend safe", async () => {
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        throw new TypeError("connection refused");
      },
    });

    for (const [targetSessionID, idempotencyKey] of [
      ["ses_target", "retry-1"],
      ["ses.dotted", "0f6c1e3a-7d2b-4c8e-9a51-3b7e2d4f6a80"],
    ] as const) {
      const error = await client
        .send({ sourceSessionID: "ses_sender", targetSessionID, message: "hello", idempotencyKey })
        .then(
          () => undefined,
          (thrown: unknown) => thrown
        );

      expect(error).toMatchObject({ resendSafe: false });
      expect((error as Error).message).toEndWith(
        `Envoy cannot recognise a resend under idempotency_key ${idempotencyKey} as a repeat ` +
          "(the key is not a lower-case UUID, or the session id holds a dot): ask the recipient " +
          "whether it arrived before sending it again."
      );
    }
  });

  test("a send whose last attempt the listener answered with a 5xx is not confirmed delivered", async () => {
    const refusal = () =>
      Response.json(
        { error: "envoy: the message bus did not answer: context deadline exceeded" },
        { status: 503 }
      );
    const recorded = recordFetch([refusal(), refusal()]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(2);
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    const unconfirmed = error as EnvoySendUnconfirmedError;
    expect(unconfirmed.last).toBeInstanceOf(EnvoyApiError);
    expect((unconfirmed.last as EnvoyApiError).details.status).toBe(503);
    expect(unconfirmed.message).toStartWith(
      "The send to ses_target is not confirmed delivered: the listener answered 503 " +
        "(envoy: the message bus did not answer: context deadline exceeded) on the last of 2 " +
        "attempts. It may or may not have arrived. Sending it again with idempotency_key "
    );
  });

  test("a send the listener refused with a 4xx is that refusal, sent once", async () => {
    const recorded = recordFetch([
      Response.json({ error: "no live session ses_target" }, { status: 404 }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(1);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect(error).not.toBeInstanceOf(EnvoySendUnconfirmedError);
    expect((error as Error).message).toBe("no live session ses_target");
  });

  test("a refusal whose body does not arrive is still the listener's refusal, sent once", async () => {
    let calls = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        calls += 1;
        const body = new ReadableStream({
          pull(controller) {
            controller.error(new TypeError("body lost"));
          },
        });
        return new Response(body, { status: 409 });
      },
    });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(calls).toBe(1);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect((error as EnvoyApiError).details.status).toBe(409);
  });

  test("a resend under the named key sends that key and reports the listener's repeat", async () => {
    const key = "0f6c1e3a-7d2b-4c8e-9a51-3b7e2d4f6a80";
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-resent",
        source: "agent",
        source_event_id: "agent.ses_sender.event-resent",
        source_session: "ses_sender",
        topic: "notifications.agent.ses_target",
        dedupe_key: `agent.ses_target.${key}`,
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-resent",
        recipient: "ses_target",
        duplicate: true,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hello",
      idempotencyKey: key,
    });

    expect(await recorded.requests[0]?.json()).toMatchObject({ idempotency_key: key });
    expect(result).toMatchObject({ recipient: "ses_target", confirmed: true, duplicate: true });
  });

  test("retries a delivered publish with one generated idempotency key", async () => {
    const idempotencyKeys: string[] = [];
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async (_input, init) => {
        const body = JSON.parse(String(init?.body)) as { readonly idempotency_key?: string };
        if (typeof body.idempotency_key !== "string") {
          return new Response("idempotency key required", { status: 400 });
        }
        idempotencyKeys.push(body.idempotency_key);
        if (idempotencyKeys.length === 1) {
          throw new TypeError("response lost after delivery");
        }
        if (body.idempotency_key !== idempotencyKeys[0]) {
          return new Response("duplicate message", { status: 409 });
        }
        return jsonResponse({
          event_id: "event-published",
          source: "agent",
          source_event_id: "agent.sender.event-published",
          source_session: "ses_sender",
          topic: "notifications.role.legion-reviewer",
          dedupe_key: "event-published",
          issued_at: 1,
          payload_summary: "review this",
          trace_id: "trace-published",
        });
      },
    });

    await client.publish({
      sourceSessionID: "ses_sender",
      topic: "notifications.role.legion-reviewer",
      message: "review this",
    });

    expect(idempotencyKeys).toHaveLength(2);
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);
  });

  test("serializes all additive message fields and retains role holder results", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-role",
        source: "agent",
        source_event_id: "agent.sender.event-role",
        source_session: "01a00000-0000-7000-0000-000000000001",
        topic: "notifications.role.legion-reviewer",
        dedupe_key: "event-role",
        issued_at: 1,
        payload_summary: "review this",
        trace_id: "trace-role",
        holder: "01a01111-2222-7333-4444-555555555555",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.publish({
      sourceSessionID: "01a00000-0000-7000-0000-000000000001",
      topic: "notifications.role.legion-reviewer",
      message: "review this",
      inReplyTo: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expectsReply: "required",
      expiresAt: Date.parse("2026-09-07T05:00:00Z"),
    });

    expect(await recorded.requests[0]?.json()).toMatchObject({
      in_reply_to: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expects_reply: "required",
      expires_at: Date.parse("2026-09-07T05:00:00Z"),
    });
    expect(result).toMatchObject({
      envelope: { event_id: "event-role" },
      holder: "01a01111-2222-7333-4444-555555555555",
    });
  });

  test("uses listener directory and title session filters and gets a live role holder", async () => {
    const recorded = recordFetch([
      jsonResponse([
        {
          session_id: "01a00000-0000-7000-0000-000000000001",
          machine_id: "example-host",
          dir: "/work/example",
          port: 0,
          title: "Reviewer",
          topics: ["notifications.role.legion-reviewer"],
          roles: ["legion-reviewer"],
          self_subscribed: true,
          last_seen: 42,
        },
      ]),
      jsonResponse({
        role: "legion-reviewer",
        holder: "01a00000-0000-7000-0000-000000000001",
        last_seen: 42,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions({ directory: "example", title: "Review" });
    const role = await client.getRole("legion-reviewer");

    expect(sessions[0]).toMatchObject({ roles: ["legion-reviewer"], last_seen: 42 });
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/sessions?dir=example&title=Review");
    expect(role).toEqual({
      role: "legion-reviewer",
      holder: "01a00000-0000-7000-0000-000000000001",
      last_seen: 42,
    });
    expect(recorded.requests[1]?.url).toBe("http://listener/v1/roles/legion-reviewer");
  });

  test("surfaces listener error text and expected fields without retrying a 400", async () => {
    const recorded = recordFetch([
      Response.json(
        {
          error: "message must not be empty",
          expected: ["message"],
        },
        { status: 400 }
      ),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await expect(
      client.publish({
        sourceSessionID: "01a00000-0000-7000-0000-000000000001",
        topic: "notifications.role.legion-reviewer",
        message: "",
        urgency: "low",
      })
    ).rejects.toThrow("message must not be empty (expected: message)");
    expect(recorded.requests).toHaveLength(1);
  });
  test("sends the token it is given as a bearer token to the listener", async () => {
    const recorded = recordFetch([new Response(null, { status: 200 })]);
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: recorded.fetch,
      token: "listener-token",
    });

    await client.unregisterSession({ sessionID: "ses_closed" });

    expect(recorded.requests[0]?.headers.get("Authorization")).toBe("Bearer listener-token");
  });

  test("sends no Authorization header without a token, whatever ENVOY_TOKEN says", async () => {
    const previous = process.env.ENVOY_TOKEN;
    process.env.ENVOY_TOKEN = "environment-token";
    try {
      const recorded = recordFetch([new Response(null, { status: 200 })]);
      const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

      await client.unregisterSession({ sessionID: "ses_closed" });

      expect(recorded.requests[0]?.headers.has("Authorization")).toBe(false);
    } finally {
      if (previous === undefined) delete process.env.ENVOY_TOKEN;
      else process.env.ENVOY_TOKEN = previous;
    }
  });

  test("asks for a replay in the listener's request shape and parses its answer", async () => {
    const answer = {
      stream: { created: "2026-10-01T00:00:00Z", first_seq: 3, last_seq: 9 },
      subjects: [
        {
          subject: "notifications.agent.ses_a",
          messages: [
            {
              seq: 7,
              subject: "notifications.agent.ses_a",
              time: "2026-10-26T12:00:00.5Z",
              data: "{}",
            },
          ],
          next_after: 9,
          more: false,
        },
        {
          subject: "notifications.team.deploy",
          messages: [],
          next_after: 0,
          more: false,
          refused: "not_retained",
        },
      ],
    };
    const recorded = recordFetch([jsonResponse(answer)]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.replay({
      sessionID: "ses_a",
      subjects: [
        { subject: "notifications.agent.ses_a", after: 4 },
        { subject: "notifications.team.deploy" },
      ],
      maxMessages: 50,
    });

    expect(result).toEqual(answer);
    expect(recorded.requests[0]?.method).toBe("POST");
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/replay");
    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_a",
      subjects: [
        { subject: "notifications.agent.ses_a", after: 4 },
        { subject: "notifications.team.deploy" },
      ],
      max_messages: 50,
    });
  });

  // A replay runs again on the session's next trigger, and a listener that cannot replay must be
  // reported at once, so a 503 is not sent a second time.
  test("gives up a replay at its first 503, with the listener's status and code", async () => {
    const recorded = recordFetch([
      Response.json(
        { code: "REPLAY_UNAVAILABLE", error: "nats: permissions violation" },
        { status: 503 }
      ),
      Response.json({ error: "unexpected second request" }, { status: 500 }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const failure = await client
      .replay({ sessionID: "ses_a", subjects: [{ subject: "notifications.agent.ses_a" }] })
      .catch((error: unknown) => error);

    expect(failure).toBeInstanceOf(EnvoyApiError);
    expect((failure as EnvoyApiError).details.status).toBe(503);
    expect((failure as EnvoyApiError).code).toBe("REPLAY_UNAVAILABLE");
    expect((failure as EnvoyApiError).message).toBe("nats: permissions violation");
    expect(recorded.requests).toHaveLength(1);
  });
});

describe("newClientInstanceID", () => {
  test("mints 128 random bits as 32 lower-case hex characters", () => {
    expect(newClientInstanceID()).toMatch(/^[0-9a-f]{32}$/);
  });

  test("mints a different id for each client instance", () => {
    expect(newClientInstanceID()).not.toBe(newClientInstanceID());
  });
});

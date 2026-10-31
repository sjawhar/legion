import { describe, expect, test } from "bun:test";
import { machineID } from "../machine";
import {
  createEnvoyClient,
  type DeliveryAnswer,
  EnvoyApiError,
  parseSubscribeAnswer,
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
  const CONSUMER: DeliveryAnswer = {
    mode: "consumer",
    subject: "notifications.envoy.deliver.ses_sender",
    stream_id: "stream-1",
    gap: false,
  };
  const DELIVERY_REQUEST = {
    consumer: true,
    epoch: "epoch-1",
    resume: { stream_id: "stream-1", seq: 41 },
  } as const;

  test("posts a subscription using the listener request shape", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.agent.ses_sender"],
        delivery: CONSUMER,
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
      delivery: DELIVERY_REQUEST,
      plugin: PLUGIN,
    });

    expect(interest).toEqual({
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      updated_at: undefined,
      delivery: CONSUMER,
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
      delivery: DELIVERY_REQUEST,
      host: machineID(),
      plugin: "@sjawhar/pi-envoy",
      plugin_version: "11.0.0",
    });
  });

  test("asks for consumer delivery and reads the consumer answer", async () => {
    const interest = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
    };
    const recorded = recordFetch([jsonResponse({ ...interest, delivery: CONSUMER })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 0,
      title: "",
      driving: false,
      selfSubscribed: true,
      delivery: DELIVERY_REQUEST,
      plugin: PLUGIN,
    });

    expect(result.delivery).toEqual(CONSUMER);
    // The registration carries the plugin beside the delivery request.
    expect(await recorded.requests[0]?.json()).toMatchObject({
      self_subscribed: true,
      delivery: DELIVERY_REQUEST,
      plugin: "@sjawhar/pi-envoy",
      plugin_version: "11.0.0",
    });
  });

  // A session receives only what its own consumer holds, so an answer naming no consumer fails
  // the registration here rather than leaving the session registered and deaf.
  test("fails a registration whose answer names no consumer, with the listener's reason", async () => {
    const interest = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
    };
    const recorded = recordFetch([
      jsonResponse(interest),
      jsonResponse({ ...interest, delivery: { mode: "core", reason: "no_grant" } }),
      jsonResponse({ ...interest, delivery: { mode: "push" } }),
      jsonResponse({ ...interest, delivery: 7 }),
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
        delivery: DELIVERY_REQUEST,
        plugin: PLUGIN,
      });

    // A listener of an earlier release answers no `delivery` field at all.
    await expect(register()).rejects.toThrow(/answered no delivery for ses_sender/);
    await expect(register()).rejects.toThrow(/answered delivery mode core \(no_grant\)/);
    await expect(register()).rejects.toThrow(/answered delivery mode push/);
    await expect(register()).rejects.toThrow(/cannot read/);
  });

  test("parseSubscribeAnswer throws on an absent, core or unreadable delivery answer", () => {
    const interest = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
    };

    expect(parseSubscribeAnswer({ ...interest, delivery: CONSUMER })).toEqual({
      ...interest,
      updated_at: undefined,
      delivery: CONSUMER,
    });
    expect(() => parseSubscribeAnswer(interest)).toThrow(
      "the listener answered no delivery for ses_sender: it predates session consumers, and a session receives only what its own consumer holds"
    );
    expect(() => parseSubscribeAnswer({ ...interest, delivery: null })).toThrow(
      /answered no delivery/
    );
    expect(() =>
      parseSubscribeAnswer({ ...interest, delivery: { mode: "core", reason: "not_requested" } })
    ).toThrow(
      "the listener gave ses_sender no consumer: it answered delivery mode core (not_requested), and a session receives only what its own consumer holds"
    );
    expect(() => parseSubscribeAnswer({ ...interest, delivery: "consumer" })).toThrow(
      "the listener gave ses_sender no consumer: it answered a delivery field this build cannot read, and a session receives only what its own consumer holds"
    );
    // A consumer answer missing the subject the relay arrives on is unreadable too.
    expect(() =>
      parseSubscribeAnswer({
        ...interest,
        delivery: { mode: "consumer", stream_id: "s", gap: false },
      })
    ).toThrow(/answered delivery mode consumer/);
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
        delivery: CONSUMER,
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
      delivery: DELIVERY_REQUEST,
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
        delivery: CONSUMER,
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
      delivery: DELIVERY_REQUEST,
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
          delivery: DELIVERY_REQUEST,
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

    await client.unregisterSession("ses_closed");

    expect(recorded.requests).toHaveLength(1);
    expect(recorded.requests[0]?.method).toBe("DELETE");
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/sessions/ses_closed");
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

  test("keeps each session's delivery mode apart from a listener that reports none", async () => {
    const row = {
      machine_id: "host-a",
      dir: "/work",
      port: 13381,
      title: "Envoy QA",
      topics: [],
      self_subscribed: false,
    };
    const relayed = { ...row, session_id: "ses_relayed", delivery: "consumer" };
    // A listener that reports delivery writes the key for every session, so "" is the core path
    // and an absent key is a listener older than the field: a measurement of how much traffic is
    // still on core counts them differently, and neither may become the other here.
    const core = { ...row, session_id: "ses_core", delivery: "" };
    const unreported = { ...row, session_id: "ses_unreported" };
    const recorded = recordFetch([jsonResponse([relayed, core, unreported])]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    expect(sessions).toEqual([relayed, core, unreported]);
    expect(sessions.map((session) => session.delivery)).toEqual(["consumer", "", undefined]);
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

    await client.unregisterSession("ses_closed");

    expect(recorded.requests[0]?.headers.get("Authorization")).toBe("Bearer listener-token");
  });

  test("sends no Authorization header without a token, whatever ENVOY_TOKEN says", async () => {
    const previous = process.env.ENVOY_TOKEN;
    process.env.ENVOY_TOKEN = "environment-token";
    try {
      const recorded = recordFetch([new Response(null, { status: 200 })]);
      const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

      await client.unregisterSession("ses_closed");

      expect(recorded.requests[0]?.headers.has("Authorization")).toBe(false);
    } finally {
      if (previous === undefined) delete process.env.ENVOY_TOKEN;
      else process.env.ENVOY_TOKEN = previous;
    }
  });
});

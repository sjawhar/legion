import { describe, expect, test } from "bun:test";
import { createEnvoyClient, EnvoyApiError } from "../transport";

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
    });

    expect(interest).toEqual({
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      updated_at: undefined,
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
    });
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
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const envelope = await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hello",
    });

    expect(envelope.topic).toBe("notifications.agent.ses_target");
    expect(await recorded.requests[0]?.json()).toEqual({
      source_session: "ses_sender",
      target_session: "ses_target",
      message: "hello",
    });
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

    await client.publish({
      sourceSessionID: "ses_sender",
      topic: "notifications.role.controller",
      message: "broadcast",
    });

    expect(await recorded.requests[0]?.json()).toEqual({
      source_session: "ses_sender",
      topic: "notifications.role.controller",
      message: "broadcast",
    });
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

    const interest = await client.setRole({ sessionID: "ses_sender", role: "controller" });

    expect(interest.topics).toEqual(["notifications.role.controller"]);
    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_sender",
      role: "controller",
    });
  });

  test("lists listener sessions and retains their registration metadata", async () => {
    const recorded = recordFetch([
      jsonResponse([
        {
          session_id: "ses_sender",
          machine_id: "host-a",
          dir: "/work",
          port: 13381,
          title: "Envoy QA",
          topics: ["notifications.agent.ses_sender"],
          self_subscribed: false,
        },
      ]),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    expect(sessions).toEqual([
      {
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        port: 13381,
        title: "Envoy QA",
        topics: ["notifications.agent.ses_sender"],
        self_subscribed: false,
      },
    ]);
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
});

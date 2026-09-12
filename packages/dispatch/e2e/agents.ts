export interface FakeSession {
  session_id: string;
  title: string;
  dir?: string;
  machine_id?: string;
  roles?: string[];
  capabilities?: string[];
  last_seen?: number;
  port?: number;
  topics?: string[];
}

async function fixtureRequest(
  path: string,
  method: "GET" | "PATCH" | "PUT",
  body?: object
): Promise<Response> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("live Envoy fixtures are unavailable with PLAYWRIGHT_BASE_URL");
  }
  const response = await fetch(`http://127.0.0.1:${process.env.FAKE_ENVOY_PORT ?? "9021"}${path}`, {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    method,
  });
  if (!response.ok) {
    throw new Error(`${method} ${path} failed: ${response.status} ${await response.text()}`);
  }
  return response;
}

export async function setLiveSessions(rows: FakeSession[]): Promise<void> {
  await fixtureRequest("/__fixture/sessions", "PUT", rows);
}

export async function setSessionLive(sessionID: string, live: boolean): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", { live });
}

export async function setSessionSendStatus(sessionID: string, status: 200 | 404): Promise<void> {
  await fixtureRequest(`/__fixture/sessions/${encodeURIComponent(sessionID)}`, "PATCH", {
    send_status: status,
  });
}

export async function getSentMessages(): Promise<Record<string, unknown>[]> {
  return (await fixtureRequest("/__fixture/sends", "GET")).json() as Promise<
    Record<string, unknown>[]
  >;
}

export interface FakeInterest {
  session_id: string;
  topics: string[];
  updated_at?: number;
}

export async function setInterests(rows: FakeInterest[]): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("live Envoy fixtures are unavailable with PLAYWRIGHT_BASE_URL");
  }

  const response = await fetch(
    `http://127.0.0.1:${process.env.FAKE_ENVOY_PORT ?? "9021"}/__fixture/interests`,
    {
      body: JSON.stringify(rows),
      headers: { "Content-Type": "application/json" },
      method: "PUT",
    }
  );
  if (!response.ok) {
    throw new Error(`setting interests failed: ${response.status} ${await response.text()}`);
  }
}

export async function getUnsubscribeCalls(): Promise<{ session_id: string; topics: string[] }[]> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("live Envoy fixtures are unavailable with PLAYWRIGHT_BASE_URL");
  }

  const response = await fetch(
    `http://127.0.0.1:${process.env.FAKE_ENVOY_PORT ?? "9021"}/__fixture/unsubscribe-calls`
  );
  if (!response.ok) {
    throw new Error(
      `reading unsubscribe calls failed: ${response.status} ${await response.text()}`
    );
  }
  return (await response.json()) as { session_id: string; topics: string[] }[];
}

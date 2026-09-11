export interface FakeSession {
  session_id: string;
  title: string;
  dir?: string;
  machine_id?: string;
  roles?: string[];
  last_seen?: number;
  port?: number;
  topics?: string[];
}

export async function setLiveSessions(rows: FakeSession[]): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error("live Envoy fixtures are unavailable with PLAYWRIGHT_BASE_URL");
  }

  const response = await fetch(
    `http://127.0.0.1:${process.env.FAKE_ENVOY_PORT ?? "9021"}/__fixture/sessions`,
    {
      body: JSON.stringify(rows),
      headers: { "Content-Type": "application/json" },
      method: "PUT",
    }
  );
  if (!response.ok) {
    throw new Error(`setting live sessions failed: ${response.status} ${await response.text()}`);
  }
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

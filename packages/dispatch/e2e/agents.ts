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

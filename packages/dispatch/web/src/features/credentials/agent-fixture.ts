import type { Agent } from "../../api/types";

/** One live session, as `GET /api/v1/agents` lists it - shared by every credentials test that
 *  needs a running agent to resolve a session id against (session.test.ts,
 *  CredentialRequestsSection.test.tsx, CredentialRecordPage.credential.test.tsx). */
export function runningAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    capabilities: [],
    dir: "/home/alice/legion",
    last_activity: null,
    last_seen: 1,
    machine_id: "devbox-alice",
    open_asks: 0,
    roles: [],
    session_id: "sess-1",
    title: "Reviewing LEGION-587",
    ...overrides,
  };
}

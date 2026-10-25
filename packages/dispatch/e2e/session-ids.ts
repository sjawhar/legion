// The session ids the e2e seeds give the sessions they name by role: the local instance's
// workspace (`workspace.ts`), the Agents fixtures (`agents.ts`) and the specs that seed those
// sessions themselves. An Envoy session id is a UUID, and Dispatch's agent routes refuse anything
// else (`agentStreamSessionID`, 400 `SESSION_ID_INPUT`), so a session seeded under a name would
// list on the Agents page and have no live view. Each first segment differs, so the short form an
// untitled session reads as (`session:019a1c01…`, `shortSessionId`) differs too. This module
// imports nothing, so a script outside the harness (the docs' media rig) reads it without starting
// anything.
export const sessionIds = {
  planner: "019a1c01-0d7b-7a2a-8e93-4a5b6c7d8e01",
  tester: "019a1c02-1e9c-7b3b-9f74-3b6c0d3e4f02",
  reviewer: "019a1c03-2f0d-7c4c-ab85-0c7d1e4f5a03",
  observer: "019a1c04-3a1e-7b5d-9c06-1d8e2f5a6b04",
  archivist: "019a1c05-4b2f-7c6e-8d17-2e9f3a6b7c05",
  silent: "019a1c06-5c3a-7d7f-ae28-3f0a4b7c8d06",
} as const;

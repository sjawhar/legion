import { expect, test } from "bun:test";

import type { CredentialSession } from "../../api/types";
import { runningAgent } from "./agent-fixture";
import { credentialSessionLines, credentialSessionNamesAnyone } from "./session";

const RUNNING = {
  kind: "running",
  place: { dir: "/home/alice/legion", machineId: "devbox-alice" },
} as const;

test("credentialSessionNamesAnyone is false for undefined, both-null, and true once either id is set", () => {
  expect(credentialSessionNamesAnyone(undefined)).toBe(false);
  expect(credentialSessionNamesAnyone({ enrollment: null, request: null })).toBe(false);
  expect(credentialSessionNamesAnyone({ enrollment: "sess-1", request: null })).toBe(true);
  expect(credentialSessionNamesAnyone({ enrollment: null, request: "sess-1" })).toBe(true);
});

test("no session named: neither id set, including an older broker's response with no session field at all", () => {
  expect(credentialSessionLines(undefined, [], false)).toEqual([]);
  expect(
    credentialSessionLines({ enrollment: null, request: null }, [runningAgent()], false)
  ).toEqual([]);
});

test("one line, no source, when only the enrollment's id is set and it is live", () => {
  const session: CredentialSession = { enrollment: "sess-1", request: null };
  const lines = credentialSessionLines(session, [runningAgent()], false);
  expect(lines).toEqual([{ id: "sess-1", status: RUNNING, source: null }]);
});

test("one line, no source, when both ids are set but agree", () => {
  const session: CredentialSession = { enrollment: "sess-1", request: "sess-1" };
  const lines = credentialSessionLines(session, [runningAgent()], false);
  expect(lines).toHaveLength(1);
  expect(lines[0]?.source).toBeNull();
  expect(lines[0]?.id).toBe("sess-1");
});

// A request-only id - the common host-session case - resolves as a `request`-sourced line, so the
// renderer can tell it from the enrollment's; it carries no preamble on screen.
test("one line, source request, when only the request's id is set - the common host-session case", () => {
  const session: CredentialSession = { enrollment: null, request: "sess-1" };
  const lines = credentialSessionLines(session, [runningAgent()], false);
  expect(lines).toEqual([{ id: "sess-1", status: RUNNING, source: "request" }]);
});

test("not running when the agents list answered and the id isn't in it", () => {
  const session: CredentialSession = { enrollment: "sess-gone", request: null };
  const lines = credentialSessionLines(session, [], false);
  expect(lines).toEqual([{ id: "sess-gone", source: null, status: { kind: "not-running" } }]);
});

// The agents list never answered, or its latest read failed (Envoy unavailable or not
// configured): even an id the last list held is unknown, since that list proves nothing now.
test("unknown while unresolved, even for an id the last agents list held", () => {
  const session: CredentialSession = { enrollment: "sess-1", request: null };
  const lines = credentialSessionLines(session, [runningAgent()], true);
  expect(lines).toEqual([{ id: "sess-1", source: null, status: { kind: "unknown" } }]);
});

test("two lines, each naming where its id came from, when both are set and differ", () => {
  const session: CredentialSession = { enrollment: "sess-enrollment", request: "sess-request" };
  const lines = credentialSessionLines(
    session,
    [runningAgent({ session_id: "sess-enrollment" })],
    false
  );
  expect(lines).toEqual([
    { id: "sess-enrollment", status: RUNNING, source: "enrollment" },
    { id: "sess-request", source: "request", status: { kind: "not-running" } },
  ]);
});

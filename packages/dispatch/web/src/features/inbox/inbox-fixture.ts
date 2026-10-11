import { afterEach, beforeEach, type Mock, spyOn } from "bun:test";

import { api } from "../../api/client";
import type { InboxRow } from "../../api/types";

/** The mocks every Inbox render needs: the signed-in login, and the credential requests waiting
 *  on it; an open ask card also reads its owner's subscribers and its backlinks. */
export interface InboxApiMocks {
  whoAmI: Mock<typeof api.whoAmI>;
  getIssueSubscribers: Mock<typeof api.getIssueSubscribers>;
  getArtifactSubscribers: Mock<typeof api.getArtifactSubscribers>;
  getReferences: Mock<typeof api.getReferences>;
  getCredentialPending: Mock<typeof api.getCredentialPending>;
}

/** Installs the shared fixtures `Inbox.test.tsx` and `InboxDrawer.test.tsx` both build on: call
 *  once at module scope. The returned object is stable across the file's `beforeEach` cycles even
 *  though what it holds is replaced each time, so a test may still override one mock's resolution
 *  in place (`mocks.getCredentialPending.mockResolvedValue(...)`). */
export function installInboxApiMocks(): InboxApiMocks {
  const mocks = {} as InboxApiMocks;
  beforeEach(() => {
    window.localStorage.clear();
    mocks.whoAmI = spyOn(api, "whoAmI").mockResolvedValue({ kind: "user", login: "alice" });
    mocks.getCredentialPending = spyOn(api, "getCredentialPending").mockResolvedValue({
      pending: [],
    });
    mocks.getIssueSubscribers = spyOn(api, "getIssueSubscribers").mockResolvedValue([]);
    mocks.getArtifactSubscribers = spyOn(api, "getArtifactSubscribers").mockResolvedValue([]);
    mocks.getReferences = spyOn(api, "getReferences").mockResolvedValue({
      edges: [],
      node: { id: "", kind: "ask" },
    });
  });
  afterEach(() => {
    mocks.whoAmI.mockRestore();
    mocks.getCredentialPending.mockRestore();
    mocks.getIssueSubscribers.mockRestore();
    mocks.getArtifactSubscribers.mockRestore();
    mocks.getReferences.mockRestore();
  });
  return mocks;
}

/** A single open ask on an issue, overridable per test. */
export function issueAsk(overrides: Partial<InboxRow> = {}): InboxRow {
  return {
    anchor: null,
    answer: null,
    author: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    edited_at: null,
    id: "ask-a",
    issue: { assignee: "alice", key: "CORE-1", title: "Fix the thing" },
    issue_key: "CORE-1",
    kind: "question",
    multiple: false,
    opened_event_id: 1,
    options: [],
    thread: { answers: [], edits: [], followers: [], replies: [] },
    question: "Which approach?",
    state: "open",
    waiting_on: "human",
    priority: null,
    snoozed_until: null,
    urgency: "med",
    ...overrides,
  };
}

/** Resolves `getAsk` for each row's full thread from the same fixtures `getInbox` already seeded. */
export function mockAskReads(rows: readonly InboxRow[]) {
  return spyOn(api, "getAsk").mockImplementation(async (id: string) => {
    const ask = rows.find((row) => row.id === id);
    if (ask === undefined) throw new Error(`no fixture for ${id}`);
    return { ask, answers: [], edits: [], followers: [], replies: [] };
  });
}

import { expect, test } from "bun:test";

import { eventRefreshTargets } from "./event-refresh";
import type { Event } from "./types";

/**
 * What refreshes the question map a `#id` resolves against
 * (`["references", "questions", <kind>, <id>]`, the read behind every `#id`): a version of a
 * document, which decides which `:::ask{#id}` blocks its latest version declares, and an ask
 * settling into a block or leaving one. Nothing else: the read is held by every open body that
 * writes a `#id`, so an event that cannot move a block must not ask for it again.
 */

/** An event of `type`, with the payload its reader reads; the feed's union is wider than this
 *  test needs. */
function eventOf(type: string, payload: Record<string, unknown>, issueKey: string | null): Event {
  return {
    actor: { id: "alice", kind: "user" },
    artifact_id: typeof payload.artifact_id === "string" ? payload.artifact_id : null,
    created_at: "2026-11-05T00:00:00Z",
    id: 1,
    issue_key: issueKey,
    notify: true,
    payload,
    project: "CORE",
    seq: 1,
    type,
  } as unknown as Event;
}

/** Whether the event's targets hold the question-map read, by prefix or exactly. */
function refreshesQuestionMaps(event: Event): boolean {
  return eventRefreshTargets(event).some(
    (target) =>
      target.queryKey[0] === "references" &&
      target.queryKey[1] === "questions" &&
      target.exact !== true
  );
}

test("an ask settling into a block, and a version of its document, refresh every question map", () => {
  const settled = eventOf(
    "ask.opened",
    { block_artifact: { id: "artifact-1" }, block_id: "q2", id: "ask-1" },
    "CORE-1"
  );
  expect(refreshesQuestionMaps(settled)).toBe(true);
  const retracted = eventOf(
    "ask.resolved",
    { block_artifact: { id: "artifact-1" }, block_id: "q2", id: "ask-1" },
    "CORE-1"
  );
  expect(refreshesQuestionMaps(retracted)).toBe(true);
  const version = eventOf("artifact.version", { artifact_id: "artifact-1", number: 2 }, "CORE-1");
  expect(refreshesQuestionMaps(version)).toBe(true);
  // A project document's own version and asks are read under the same rule.
  const documentVersion = eventOf(
    "artifact.version",
    { artifact_id: "artifact-2", number: 2 },
    null
  );
  expect(refreshesQuestionMaps(documentVersion)).toBe(true);
});

test("an event that can move no question block asks for no question map again", () => {
  const follower = eventOf("ask.follower_added", { ask_id: "ask-1" }, "CORE-1");
  expect(refreshesQuestionMaps(follower)).toBe(false);
  const reply = eventOf("comment.created", { body: "Noted.", id: "comment-1" }, "CORE-1");
  expect(refreshesQuestionMaps(reply)).toBe(false);
  const updated = eventOf("issue.updated", { changed: ["status"], key: "CORE-1" }, "CORE-1");
  expect(refreshesQuestionMaps(updated)).toBe(false);
});

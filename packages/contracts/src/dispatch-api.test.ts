import { expect, test } from "bun:test";
import { DispatchEventSchema } from "./dispatch-api";

test("accepts the typed artifact version event payload", () => {
  const event = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-09T00:00:00Z",
    id: 42,
    issue_key: "DSP-1",
    notify: true,
    payload: {
      artifact_id: "artifact-1",
      name: "spec.md",
      version: { number: 2, summary: null },
    },
    seq: 7,
    type: "artifact.version",
  };

  expect(DispatchEventSchema.safeParse(event)).toMatchObject({ success: true });
});

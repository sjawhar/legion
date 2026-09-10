import { describe, expect, it } from "bun:test";
import { DispatchDecodeFailure, dispatchIssueEvent } from "../dispatch-events";
import issueCreatedRoot from "./fixtures/dispatch/issue-created-root.json";

describe("Dispatch durable event decoding", () => {
  it("preserves an unknown nonempty event type for the reducer to acknowledge", () => {
    const decoded = dispatchIssueEvent({
      payload: JSON.stringify({
        ...issueCreatedRoot,
        type: "ask.resolved",
        payload: { id: "ask-1" },
      }),
    });

    expect(decoded).toEqual({
      type: "ask.resolved",
      key: "LEGSMOKE-1",
      seq: 1,
      notify: false,
      payload: { id: "ask-1" },
      eventId: "dispatch-208",
    });
  });

  it("rejects an issue event whose nested key disagrees with its event key", () => {
    expect(() =>
      dispatchIssueEvent({
        payload: JSON.stringify({
          ...issueCreatedRoot,
          payload: { ...issueCreatedRoot.payload, key: "LEGSMOKE-2" },
        }),
      })
    ).toThrow(DispatchDecodeFailure);
  });

  it("rejects an issue event whose parent is not a Dispatch key", () => {
    expect(() =>
      dispatchIssueEvent({
        payload: JSON.stringify({
          ...issueCreatedRoot,
          payload: { ...issueCreatedRoot.payload, parent: "acme/widgets#2" },
        }),
      })
    ).toThrow(DispatchDecodeFailure);
  });
});

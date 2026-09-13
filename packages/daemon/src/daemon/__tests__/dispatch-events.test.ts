import { describe, expect, it } from "bun:test";
import { DispatchDecodeFailure, dispatchIssueEvent } from "../dispatch-events";
import issueCreatedRoot from "./fixtures/dispatch/issue-created-root.json";
import artifactApproved from "./fixtures/dispatch/legsmoke-3-artifact.approved.json";
import artifactChangesRequested from "./fixtures/dispatch/legsmoke-3-artifact.changes_requested.json";
import artifactVersion from "./fixtures/dispatch/legsmoke-3-artifact.version.json";

function encoded(event: Record<string, unknown>, payload: Record<string, unknown>) {
  return { payload: JSON.stringify({ ...event, payload }) };
}

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

  it("decodes the three design-gate artifact events in their published shapes", () => {
    expect(dispatchIssueEvent({ payload: JSON.stringify(artifactApproved) }).type).toBe(
      "artifact.approved"
    );
    expect(dispatchIssueEvent({ payload: JSON.stringify(artifactChangesRequested) }).type).toBe(
      "artifact.changes_requested"
    );
    expect(dispatchIssueEvent({ payload: JSON.stringify(artifactVersion) }).payload).toEqual(
      artifactVersion.payload
    );
  });

  it("rejects artifact.changes_requested without a reason as poison, never a reducer input", () => {
    const { reason: _reason, ...withoutReason } = artifactChangesRequested.payload;
    expect(() => dispatchIssueEvent(encoded(artifactChangesRequested, withoutReason))).toThrow(
      DispatchDecodeFailure
    );
    expect(() =>
      dispatchIssueEvent(encoded(artifactChangesRequested, { ...withoutReason, reason: null }))
    ).toThrow("Dispatch artifact.changes_requested payload has no reason");
    expect(() =>
      dispatchIssueEvent(encoded(artifactChangesRequested, { ...withoutReason, reason: "" }))
    ).toThrow(DispatchDecodeFailure);
  });

  it("rejects a review event whose version is not a positive integer or whose artifact_id is missing", () => {
    expect(() =>
      dispatchIssueEvent(
        encoded(artifactApproved, { ...artifactApproved.payload, version: { number: 12 } })
      )
    ).toThrow("Dispatch artifact.approved payload has no positive integer version");
    expect(() =>
      dispatchIssueEvent(encoded(artifactApproved, { ...artifactApproved.payload, version: 0 }))
    ).toThrow(DispatchDecodeFailure);
    const { artifact_id: _artifactId, ...withoutArtifact } = artifactApproved.payload;
    expect(() => dispatchIssueEvent(encoded(artifactApproved, withoutArtifact))).toThrow(
      "Dispatch artifact.approved payload has no artifact_id"
    );
  });

  it("rejects artifact.version whose version is a bare number or lacks a number", () => {
    expect(() =>
      dispatchIssueEvent(encoded(artifactVersion, { ...artifactVersion.payload, version: 13 }))
    ).toThrow("Dispatch artifact.version payload has no positive integer version.number");
    const { number: _number, ...versionWithoutNumber } = artifactVersion.payload.version;
    expect(() =>
      dispatchIssueEvent(
        encoded(artifactVersion, { ...artifactVersion.payload, version: versionWithoutNumber })
      )
    ).toThrow(DispatchDecodeFailure);
  });
});

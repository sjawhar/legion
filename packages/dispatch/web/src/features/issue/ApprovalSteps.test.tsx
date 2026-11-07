import { expect, test } from "bun:test";
import { screen, within } from "@testing-library/react";

import { renderWithQueries } from "../../__tests__/query-provider";
import type { Artifact, ArtifactApproval } from "../../api/types";
import { ApprovalSteps } from "./ApprovalSteps";
import type { StepEntry } from "./gated-steps";

const SPEC_ID = "artifact-spec";
const DECISIONS_ID = "artifact-decisions";

function doc(id: string, name: string, approval?: ArtifactApproval): Artifact {
  return {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: "CORE-1",
    kind: "doc",
    name,
    primary: id === SPEC_ID,
    project: "CORE",
    slug: name.replace(".", "-"),
    versions: [],
    ...(approval === undefined ? {} : { approval }),
  };
}

function specEntry(approval?: ArtifactApproval): StepEntry {
  return { document: doc(SPEC_ID, "spec.md", approval), label: "Spec", step: "spec" };
}

function decisionsEntry(approval?: ArtifactApproval): StepEntry {
  return {
    document: doc(DECISIONS_ID, "decisions.md", approval),
    label: "Decisions",
    step: "decisions",
  };
}

function renderSteps(entries: StepEntry[]) {
  return renderWithQueries(<ApprovalSteps entries={entries} primaryArtifactId={SPEC_ID} />);
}

function stepItem(view: { container: HTMLElement }, step: string): HTMLElement {
  const item = view.container.querySelector(`[data-approval-step="${step}"]`);
  if (item === null) {
    throw new Error(`no step item for ${step}`);
  }
  return item as HTMLElement;
}

test("lists one named item per entry, in the entries' order, under one Approvals list", () => {
  const view = renderSteps([
    decisionsEntry({ latest_version: 1, state: "awaiting" }),
    { document: undefined, label: "Verification", step: "criteria" },
    specEntry({ latest_version: 2, state: "approved", version: 2 }),
  ]);
  try {
    const list = screen.getByRole("list", { name: "Approvals" });
    expect(list.dataset.testid).toBe("issue-approval-steps");
    const steps = [...view.container.querySelectorAll("[data-approval-step]")].map(
      (item) => (item as HTMLElement).dataset.approvalStep
    );
    expect(steps).toEqual(["decisions", "criteria", "spec"]);
    expect(
      within(list)
        .getAllByRole("listitem")
        .map((item) => item.textContent)
    ).toEqual([
      "DecisionsAwaiting approval",
      "VerificationNot requested",
      // The primary document keeps the header's actions; an approved document offers only one.
      "SpecApproved v2Request changes",
    ]);
  } finally {
    view.unmount();
  }
});

test("each approval state reads as that step's state", () => {
  const cases: Array<[ArtifactApproval, string]> = [
    [{ latest_version: 1, state: "draft" }, "Not requested"],
    [{ latest_version: 1, state: "awaiting" }, "Awaiting approval"],
    [{ latest_version: 3, state: "approved", version: 3 }, "Approved v3"],
    [{ latest_version: 4, state: "stale", version: 3 }, "Approved v3 · changed since"],
    [{ latest_version: 2, state: "changes_requested", version: 2 }, "Changes requested"],
  ];
  for (const [approval, label] of cases) {
    const view = renderSteps([decisionsEntry(approval)]);
    try {
      expect(within(stepItem(view, "decisions")).getByText(label)).not.toBeNull();
    } finally {
      view.unmount();
    }
  }
});

test("a step with no document and a document with no approval both read Not requested", () => {
  const view = renderSteps([
    { document: undefined, label: "Verification", step: "criteria" },
    specEntry(undefined),
  ]);
  try {
    expect(within(stepItem(view, "criteria")).getByText("Not requested")).not.toBeNull();
    expect(within(stepItem(view, "spec")).getByText("Not requested")).not.toBeNull();
    // Nothing to open: no review was ever given on either document.
    expect(screen.queryAllByRole("button")).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("the primary document's entry carries the actions and no other entry does", () => {
  const view = renderSteps([
    decisionsEntry({ latest_version: 1, state: "awaiting" }),
    specEntry({ latest_version: 1, state: "awaiting" }),
  ]);
  try {
    const spec = within(stepItem(view, "spec"));
    expect(spec.getByRole("button", { name: "Approve" })).not.toBeNull();
    expect(spec.getByRole("button", { name: "Request changes" })).not.toBeNull();
    const decisions = within(stepItem(view, "decisions"));
    expect(decisions.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(decisions.queryByRole("button", { name: "Request changes" })).toBeNull();
    // One Approve and one Request changes in the header, as before the list.
    expect(screen.getAllByRole("button", { name: "Approve" })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Request changes" })).toHaveLength(1);
  } finally {
    view.unmount();
  }
});

test("a draft primary document reads `Not requested` and carries no actions", () => {
  // `ApprovalChip` draws nothing at all for a draft, actions included, so the primary
  // document's step reads the dashed pill and no action sits beside it, exactly as the
  // header's one chip does today.
  const view = renderSteps([specEntry({ latest_version: 1, state: "draft" })]);
  try {
    expect(within(stepItem(view, "spec")).getByText("Not requested")).not.toBeNull();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Request changes" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a changes-requested step says who sent it back, on which version, and why", () => {
  const view = renderSteps([
    decisionsEntry({
      by: { id: "alice", kind: "user" },
      latest_version: 3,
      reason: "Name the rollback step.",
      state: "changes_requested",
      version: 2,
    }),
    specEntry({ latest_version: 1, state: "awaiting" }),
  ]);
  try {
    const line = view.container.querySelector('[data-approval-step-reason="decisions"]');
    expect(line?.textContent).toBe(
      "Decisions: alice requested changes on v2 · Name the rollback step."
    );
    // The whole reason is readable on a phone: no tooltip standing in for clipped text.
    expect(line?.getAttribute("title")).toBeNull();
    // Only the step that was sent back gets a line.
    expect(view.container.querySelector('[data-approval-step-reason="spec"]')).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a reason line leaves out an absent reason and an absent reviewer", () => {
  const view = renderSteps([
    decisionsEntry({
      by: { id: "alice", kind: "user" },
      latest_version: 2,
      reason: null,
      state: "changes_requested",
      version: 2,
    }),
    specEntry({
      latest_version: 4,
      reason: "Split the rollout.",
      state: "changes_requested",
      version: 4,
    }),
  ]);
  try {
    expect(
      view.container.querySelector('[data-approval-step-reason="decisions"]')?.textContent
    ).toBe("Decisions: alice requested changes on v2");
    expect(view.container.querySelector('[data-approval-step-reason="spec"]')?.textContent).toBe(
      "Spec: Changes requested on v4 · Split the rollout."
    );
  } finally {
    view.unmount();
  }
});

import { expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";

import { api } from "../../api/client";
import type { Artifact, ArtifactApproval, ArtifactReview } from "../../api/types";
import { ApprovalChip } from "./ApprovalChip";

function artifact(approval: ArtifactApproval | undefined): Artifact {
  return {
    created_at: "2026-09-09T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id: "artifact-1",
    issue_key: "CORE-1",
    kind: "doc",
    name: "Design notes",
    primary: false,
    project: "CORE",
    slug: "design-notes",
    versions: [],
    ...(approval === undefined ? {} : { approval }),
  };
}

function renderChip(target: Artifact, variant?: "header" | "list") {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <ApprovalChip artifact={target} variant={variant} />
    </QueryClientProvider>
  );
}

test("renders nothing when the artifact carries no approval", () => {
  const view = renderChip(artifact(undefined), "header");
  try {
    expect(view.container.firstChild).toBeNull();
  } finally {
    view.unmount();
  }
});

test("renders nothing for a draft document in list variant", () => {
  const view = renderChip(artifact({ latest_version: 1, state: "draft" }), "list");
  try {
    expect(view.container.firstChild).toBeNull();
  } finally {
    view.unmount();
  }
});

test("shows Draft in the header variant even though lists stay quiet", () => {
  const view = renderChip(artifact({ latest_version: 1, state: "draft" }), "header");
  try {
    expect(screen.getByRole("button", { name: "Draft" })).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("renders each approval state's label", () => {
  const cases: Array<[ArtifactApproval, string]> = [
    [{ latest_version: 3, state: "awaiting" }, "Awaiting approval"],
    [{ latest_version: 3, state: "approved", version: 3 }, "Approved v3"],
    [{ latest_version: 3, state: "changes_requested", version: 2 }, "Changes requested"],
  ];
  for (const [approval, label] of cases) {
    const view = renderChip(artifact(approval), "list");
    try {
      expect(screen.getByRole("button", { name: label })).not.toBeNull();
    } finally {
      view.unmount();
    }
  }
});

test("a stale approval's label and title carry both the approved and latest version", () => {
  const view = renderChip(artifact({ latest_version: 5, state: "stale", version: 2 }), "list");
  try {
    const chip = screen.getByRole("button", { name: "Approved v2 · changed since" });
    expect(chip.title).toContain("version 2");
    expect(chip.title).toContain("is 5");
  } finally {
    view.unmount();
  }
});

test("the header hides Approve for a current approval but offers Request changes", () => {
  const view = renderChip(artifact({ latest_version: 3, state: "approved", version: 3 }), "header");
  try {
    expect(screen.queryByRole("button", { name: /^Approve( v\d+)?$/ })).toBeNull();
    expect(screen.getByRole("button", { name: "Request changes" })).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("the header's Approve reads the latest version for a stale approval", () => {
  const view = renderChip(artifact({ latest_version: 4, state: "stale", version: 2 }), "header");
  try {
    expect(screen.getByRole("button", { name: "Approve v4" })).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("clicking Approve posts an approved review and refreshes the issue and inbox", async () => {
  const createArtifactReview = spyOn(api, "createArtifactReview").mockResolvedValue({
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-1",
    ask_id: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    reason: null,
    state: "approved",
    version: 3,
  });
  const view = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header");
  try {
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
    expect(createArtifactReview).toHaveBeenCalledWith("artifact-1", { state: "approved" });
  } finally {
    createArtifactReview.mockRestore();
    view.unmount();
  }
});

test("Request changes requires a reason before it can submit, then posts it", async () => {
  const createArtifactReview = spyOn(api, "createArtifactReview").mockResolvedValue({
    actor: { id: "alice", kind: "user" },
    artifact_id: "artifact-1",
    ask_id: null,
    created_at: "2026-09-11T00:00:00Z",
    id: "review-1",
    reason: "Needs another pass",
    state: "changes_requested",
    version: 3,
  });
  const view = renderChip(artifact({ latest_version: 3, state: "awaiting" }), "header");
  try {
    fireEvent.click(screen.getByRole("button", { name: "Request changes" }));
    const dialog = screen.getByRole("dialog", { name: "Request changes" });
    const submit = within(dialog).getByRole("button", { name: "Request changes" });
    expect(submit.hasAttribute("disabled")).toBe(true);

    fireEvent.change(within(dialog).getByLabelText("Reason"), {
      target: { value: "Needs another pass" },
    });
    expect(submit.hasAttribute("disabled")).toBe(false);

    fireEvent.click(submit);
    await waitFor(() => expect(createArtifactReview).toHaveBeenCalledTimes(1));
    expect(createArtifactReview).toHaveBeenCalledWith("artifact-1", {
      reason: "Needs another pass",
      state: "changes_requested",
    });
  } finally {
    createArtifactReview.mockRestore();
    view.unmount();
  }
});

test("clicking the chip opens a popover listing every review", async () => {
  const reviews: ArtifactReview[] = [
    {
      actor: { id: "alice", kind: "user" },
      artifact_id: "artifact-1",
      ask_id: null,
      created_at: "2026-09-10T00:00:00Z",
      id: "review-1",
      reason: null,
      state: "approved",
      version: 2,
    },
    {
      actor: { id: "bob", kind: "user" },
      artifact_id: "artifact-1",
      ask_id: null,
      created_at: "2026-09-11T00:00:00Z",
      id: "review-2",
      reason: "Please tighten the intro.",
      state: "changes_requested",
      version: 3,
    },
  ];
  const listArtifactReviews = spyOn(api, "listArtifactReviews").mockResolvedValue(reviews);
  const view = renderChip(
    artifact({ latest_version: 3, state: "changes_requested", version: 3 }),
    "list"
  );
  try {
    fireEvent.click(screen.getByRole("button", { name: "Changes requested" }));
    const dialog = await screen.findByRole("dialog", { name: "Reviews" });
    expect(within(dialog).getByText(/alice approved v2/)).not.toBeNull();
    expect(within(dialog).getByText(/bob requested changes on v3/)).not.toBeNull();
    expect(
      await within(dialog).findByText((text) => text.includes("Please tighten the intro."))
    ).not.toBeNull();
  } finally {
    listArtifactReviews.mockRestore();
    view.unmount();
  }
});

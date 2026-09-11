import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import type { Artifact } from "../../api/types";

import { MarginSheet } from "./MarginSheet";

const specArtifact: Artifact = {
  created_at: "2026-09-10T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  id: "artifact-1",
  issue_key: "CORE-1",
  kind: "doc",
  name: "Spec",
  primary: true,
  slug: "spec",
  versions: [],
};

test("MarginSheet renders its tab and open ask count from its model", () => {
  const view = render(
    <MarginSheet
      model={{
        actions: {
          closeComposer: () => {},
          onAction: () => {},
          onComposerSaved: () => {},
          onReply: () => {},
          onRetryAction: () => {},
          onRetryAnsweredAsk: undefined,
          onRetryComments: () => {},
          onRetryIssue: () => {},
        },
        composer: undefined,
        items: {
          actionErrorId: undefined,
          answeredAsksPending: false,
          asksPending: false,
          comments: [],
          commentsPending: false,
          commentsError: false,
          marginRef: { current: null },
          isClosed: false,
          issueKey: "CORE-1",
          issuePending: false,
          openAskCount: 3,
          needsYou: [],
          issueError: false,
          pinned: [],
          pendingActionId: undefined,
          pinnedIds: [],
          onSelectCard: () => {},
          visibleArtifact: specArtifact,
        },
        selection: {
          hoveredItemId: undefined,
          selectedItemId: undefined,
        },
        sheet: {
          expanded: false,
          toggle: () => {},
        },
        tab: {
          set: () => {},
          value: "comments",
        },
      }}
    />
  );

  try {
    expect(screen.getByRole("button", { name: "Open review panel (3 open asks)" })).not.toBeNull();
    const margin = screen.getByTestId("margin-sheet");
    expect(margin.className).toContain("xl:sticky");
    expect(margin.className).toContain("xl:top-0");
    expect(margin.className).toContain("xl:h-auto");
    expect(margin.className).toContain("xl:max-h-dvh");
    expect(margin.className).toContain("xl:overflow-y-auto");
    const reviewItems = screen.getByLabelText("Margin review items");
    expect(reviewItems.className).not.toContain("max-h-");
    expect(reviewItems.className).not.toContain("overflow-y-auto");
    expect(screen.getByRole("tab", { name: "Comments" }).getAttribute("aria-selected")).toBe(
      "true"
    );
    expect(
      within(view.container)
        .getAllByRole("tab")
        .map((tab) => tab.textContent)
    ).toEqual(["Comments", "Pinned"]);
    expect(within(view.container).queryByRole("tab", { name: "Artifacts" })).toBeNull();
  } finally {
    view.unmount();
  }
});

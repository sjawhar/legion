import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";

import { MarginSheet } from "./MarginSheet";

test("MarginSheet renders its tab and open ask count from its model", () => {
  const view = render(
    <MarginSheet
      model={{
        actions: {
          closeComposer: () => {},
          onAction: () => {},
          onReply: () => {},
          onSelectionAction: () => {},
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
          commentListRef: { current: null },
          isClosed: false,
          issueKey: "CORE-1",
          issuePending: false,
          openAskCount: 3,
          issueError: false,
          pinned: [],
          pendingActionId: undefined,
          pinnedIds: [],
          visibleArtifact: undefined,
        },
        selection: {
          hoveredItemId: undefined,
          selectedItemId: undefined,
          value: undefined,
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
    expect(screen.getByText("3 open asks")).not.toBeNull();
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

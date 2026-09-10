import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

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
        },
        composer: undefined,
        items: {
          asksPending: false,
          comments: [],
          commentsPending: false,
          commentListRef: { current: null },
          isClosed: false,
          issueKey: "CORE-1",
          issuePending: false,
          openAskCount: 3,
          pinned: [],
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
  } finally {
    view.unmount();
  }
});

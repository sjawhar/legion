import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { Tabs } from "./Tabs";

function TabsHarness() {
  const [activeTab, setActiveTab] = useState<"issues" | "documents">("issues");
  return (
    <>
      <Tabs
        activeTab={activeTab}
        ariaLabel="Project"
        idPrefix="project"
        onSelect={setActiveTab}
        tabs={[
          { id: "issues", label: "Issues" },
          { id: "documents", label: "Documents" },
        ]}
      />
      <output data-testid="active-tab">{activeTab}</output>
    </>
  );
}

test("tablist moves focus and selection with arrow, Home, and End keys and marks aria-selected", async () => {
  const user = userEvent.setup();
  const view = render(<TabsHarness />);

  try {
    const issues = screen.getByRole("tab", { name: "Issues" });
    const documents = screen.getByRole("tab", { name: "Documents" });
    expect(issues).toHaveAttribute("aria-selected", "true");
    expect(documents).toHaveAttribute("aria-selected", "false");

    issues.focus();
    await user.keyboard("{End}");
    expect(documents).toHaveFocus();
    expect(documents).toHaveAttribute("aria-selected", "true");

    await user.keyboard("{Home}");
    expect(issues).toHaveFocus();
    expect(issues).toHaveAttribute("aria-selected", "true");

    await user.keyboard("{ArrowLeft}");
    expect(documents).toHaveFocus();
    expect(screen.getByTestId("active-tab").textContent).toBe("documents");
  } finally {
    view.unmount();
  }
});

test("a counted tab shows the count as a pill and is named `<label> (<count>)`", () => {
  const view = render(
    <Tabs
      activeTab="issues"
      ariaLabel="Project"
      idPrefix="project"
      onSelect={() => {}}
      tabs={[
        { id: "issues", label: "Issues" },
        { count: 3, id: "documents", label: "Documents" },
      ]}
    />
  );

  try {
    const documents = screen.getByRole("tab", { name: "Documents (3)" });
    expect(documents.textContent).toBe("Documents3");
    expect(screen.getByRole("tab", { name: "Issues" }).textContent).toBe("Issues");
  } finally {
    view.unmount();
  }
});

function rect(left: number, right: number): DOMRect {
  return {
    bottom: 44,
    height: 44,
    left,
    right,
    toJSON: () => ({}),
    top: 0,
    width: right - left,
    x: left,
    y: 0,
  };
}

test("a tab reached from the keyboard is scrolled into the tablist, without moving the page", async () => {
  const user = userEvent.setup();
  const view = render(<TabsHarness />);

  try {
    const tablist = screen.getByRole("tablist", { name: "Project" });
    const documents = screen.getByRole("tab", { name: "Documents" });
    // A 300 px tablist whose second tab overhangs its right edge by 60 px; the tab's rect
    // follows the tablist's scroll like a real layout would.
    tablist.getBoundingClientRect = () => rect(0, 300);
    documents.getBoundingClientRect = () =>
      rect(200 - tablist.scrollLeft, 360 - tablist.scrollLeft);
    tablist.scrollLeft = 0;
    const scrollY = window.scrollY;

    // `user.keyboard` sends to the focused element, and a click here would select the tab
    // rather than reach it from the keyboard, which is what this row measures.
    screen.getByRole("tab", { name: "Issues" }).focus();
    await user.keyboard("{ArrowRight}");

    expect(documents).toHaveFocus();
    expect(tablist.scrollLeft).toBe(60);
    expect(window.scrollY).toBe(scrollY);
  } finally {
    view.unmount();
  }
});

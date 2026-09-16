import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";
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

test("tablist moves focus and selection with arrow, Home, and End keys and marks aria-selected", () => {
  const view = render(<TabsHarness />);

  try {
    const issues = screen.getByRole("tab", { name: "Issues" });
    const documents = screen.getByRole("tab", { name: "Documents" });
    expect(issues.getAttribute("aria-selected")).toBe("true");
    expect(documents.getAttribute("aria-selected")).toBe("false");

    issues.focus();
    fireEvent.keyDown(issues, { key: "End" });
    expect(document.activeElement).toBe(documents);
    expect(documents.getAttribute("aria-selected")).toBe("true");

    fireEvent.keyDown(documents, { key: "Home" });
    expect(document.activeElement).toBe(issues);
    expect(issues.getAttribute("aria-selected")).toBe("true");

    fireEvent.keyDown(issues, { key: "ArrowLeft" });
    expect(document.activeElement).toBe(documents);
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

test("a tab reached from the keyboard is scrolled into the tablist, without moving the page", () => {
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

    fireEvent.keyDown(screen.getByRole("tab", { name: "Issues" }), { key: "ArrowRight" });

    expect(document.activeElement).toBe(documents);
    expect(tablist.scrollLeft).toBe(60);
    expect(window.scrollY).toBe(scrollY);
  } finally {
    view.unmount();
  }
});

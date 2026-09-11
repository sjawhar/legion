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

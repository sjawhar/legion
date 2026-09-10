import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { IssueTabs } from "./IssueTabs";

function TabsHarness() {
  const location = useLocation();
  return (
    <>
      <IssueTabs activeTab="spec" issueKey="CORE-1" onBeforeTabChange={() => {}} />
      <output data-testid="location">{location.pathname}</output>
    </>
  );
}

test("IssueTabs exposes the Artifacts tab", () => {
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1/spec"]}>
      <TabsHarness />
    </MemoryRouter>
  );

  try {
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Spec",
      "Log",
      "Children",
      "Artifacts",
    ]);
    fireEvent.click(screen.getByRole("tab", { name: "Artifacts" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/artifacts");
  } finally {
    view.unmount();
  }
});

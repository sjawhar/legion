import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";

import { IssueTabs } from "./IssueTabs";

function TabsHarness() {
  const location = useLocation();
  return (
    <>
      <IssueTabs
        activeTab="spec"
        artifactCount={0}
        issueKey="CORE-1"
        onBeforeTabChange={() => {}}
      />
      <output data-testid="location">{location.pathname}</output>
    </>
  );
}

test("IssueTabs exposes the Conversation tab at its canonical route", () => {
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1/spec"]}>
      <TabsHarness />
    </MemoryRouter>
  );

  try {
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Spec",
      "Conversation",
      "Children",
      "Artifacts",
    ]);
    fireEvent.click(screen.getByRole("tab", { name: "Conversation" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/conversation");
    fireEvent.click(screen.getByRole("tab", { name: "Artifacts" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/artifacts");
  } finally {
    view.unmount();
  }
});

test("IssueTabs counts the issue's uploaded artifacts on the Artifacts tab", () => {
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1/spec"]}>
      <IssueTabs
        activeTab="spec"
        artifactCount={2}
        issueKey="CORE-1"
        onBeforeTabChange={() => {}}
      />
    </MemoryRouter>
  );

  try {
    expect(screen.getByRole("tab", { name: "Artifacts (2)" })).not.toBeNull();
  } finally {
    view.unmount();
  }
});

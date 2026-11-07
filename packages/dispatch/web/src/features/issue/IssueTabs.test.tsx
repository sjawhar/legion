import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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

test("IssueTabs exposes the Conversation tab at its canonical route", async () => {
  const user = userEvent.setup();
  const view = render(
    <MemoryRouter initialEntries={["/issues/CORE-1/spec"]}>
      <TabsHarness />
    </MemoryRouter>
  );

  try {
    expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual([
      "Spec",
      "Verification",
      "Plan",
      "Conversation",
      "Children",
      "Artifacts",
      "Activity",
      "Decisions",
    ]);
    await user.click(screen.getByRole("tab", { name: "Conversation" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/conversation");
    await user.click(screen.getByRole("tab", { name: "Artifacts" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/artifacts");
    await user.click(screen.getByRole("tab", { name: "Activity" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/activity");
    await user.click(screen.getByRole("tab", { name: "Verification" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/verification");
    await user.click(screen.getByRole("tab", { name: "Plan" }));
    expect(screen.getByTestId("location").textContent).toBe("/issues/CORE-1/plan");
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

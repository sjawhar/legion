import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";
import type { Agent } from "../../api/types";
import { RecipientPicker, recipientForRoute, recipientOptions } from "./RecipientPicker";

const agents: Agent[] = [
  {
    capabilities: [],
    dir: "/w/worker",
    last_seen: 2,
    machine_id: "m1",
    roles: [],
    session_id: "B",
    title: "worker",
  },
  {
    capabilities: ["aside", "btw"],
    dir: "/w/legion",
    last_seen: 3,
    machine_id: "m1",
    roles: ["legion-planner", "legion-controller-core"],
    session_id: "A",
    title: "planner",
  },
];

test("orders controller roles before other roles and then live sessions", () => {
  expect(recipientOptions(agents).map((recipient) => recipient.target)).toEqual([
    "role:legion-controller-core",
    "role:legion-planner",
    "session:A",
    "session:B",
  ]);
  expect(recipientForRoute(agents, "role:legion-planner")?.target).toBe("role:legion-planner");
});

test("searches title directory and role while exposing recipient detail", () => {
  let selected = "";
  const view = render(
    <RecipientPicker
      agents={agents}
      onChange={(recipient) => {
        selected = recipient.target;
      }}
      onOpenChange={() => {}}
      value={null}
    />
  );

  try {
    fireEvent.click(screen.getByRole("button", { name: "Choose recipient" }));
    const dialog = screen.getByRole("dialog", { name: "Recipient picker" });
    expect(dialog.textContent).toContain("planner");
    expect(dialog.textContent).toContain("/w/legion");
    fireEvent.change(screen.getByRole("searchbox", { name: "Search recipients" }), {
      target: { value: "worker" },
    });
    expect(dialog.textContent).toContain("worker");
    expect(dialog.textContent).not.toContain("planner");
    fireEvent.click(screen.getByRole("button", { name: /worker \/w\/worker/ }));
    expect(selected).toBe("session:B");
  } finally {
    view.unmount();
  }
});

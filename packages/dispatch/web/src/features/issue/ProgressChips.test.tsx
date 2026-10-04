import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";

import { ProgressChips } from "./ProgressChips";

test("the header's bar fills to done of total and names each count", () => {
  const view = render(
    <ProgressChips
      bar
      progress={{ tasks: { done: 3, total: 4 }, children: { done: 0, total: 5 } }}
    />
  );
  try {
    const tasks = screen.getByTestId("issue-progress-tasks");
    expect(tasks.textContent).toBe("3/4 tasks");
    expect(tasks.getAttribute("title")).toContain("3 of 4 task-list items");
    const fills = [tasks, screen.getByTestId("issue-progress-children")].map(
      (chip) => (chip.querySelector("[aria-hidden] > span") as HTMLElement).style.width
    );
    expect(fills).toEqual(["75%", "0%"]);
    expect(screen.getByTestId("issue-progress-children").textContent).toBe("0/5 children");
  } finally {
    view.unmount();
  }
});

test("a count the server did not make renders nothing, and neither renders no chips at all", () => {
  const onlyChildren = render(
    <ProgressChips progress={{ tasks: null, children: { done: 2, total: 2 } }} />
  );
  try {
    expect(screen.queryByTestId("issue-progress-tasks")).toBeNull();
    expect(screen.getByTestId("issue-progress-children").textContent).toBe("2/2 children");
  } finally {
    onlyChildren.unmount();
  }
  const neither = render(<ProgressChips progress={{ tasks: null, children: null }} />);
  try {
    expect(neither.container.childElementCount).toBe(0);
  } finally {
    neither.unmount();
  }
});

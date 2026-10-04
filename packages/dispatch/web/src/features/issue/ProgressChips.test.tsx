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

test("a count of one is written in the singular, label and title alike", () => {
  const view = render(
    <ProgressChips progress={{ tasks: { done: 1, total: 1 }, children: { done: 0, total: 1 } }} />
  );
  try {
    const tasks = screen.getByTestId("issue-progress-tasks");
    expect(tasks.textContent).toBe("1/1 task");
    expect(tasks.getAttribute("title")).toBe(
      "1 of 1 task-list item in the spec (- [ ] and - [x], nested lists included) is checked."
    );
    const children = screen.getByTestId("issue-progress-children");
    expect(children.textContent).toBe("0/1 child");
    expect(children.getAttribute("title")).toBe("0 of 1 child issue is done.");
  } finally {
    view.unmount();
  }
});

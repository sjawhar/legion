import { expect, test } from "bun:test";
import { render, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import type { IssueComponents } from "../../api/types";
import { IssueComponentsLine } from "./IssueComponentsLine";

// Queries are scoped to this render's container: bun runs every test file in
// one document, and a sibling file's un-unmounted render would otherwise leak
// its links into a `screen`-wide role query.
function renderLine(components: IssueComponents) {
  const view = render(
    <MemoryRouter>
      <IssueComponentsLine components={components} />
    </MemoryRouter>
  );
  return { ...view, q: within(view.container) };
}

test("IssueComponentsLine reads Not attached when no ancestor chain attached the issue", () => {
  const view = renderLine({
    mode: "inherit",
    ids: [],
    unknown: [],
    reason: null,
    inherited_from: null,
  });
  try {
    expect(view.q.getByTestId("issue-components").textContent).toBe("Components:Not attached");
    expect(view.q.queryByRole("link")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("IssueComponentsLine lists an explicit set as chips, marks retired ids, and links the ancestor it came from", () => {
  const view = renderLine({
    mode: "explicit",
    ids: ["dispatch-server", "web"],
    unknown: ["legacy-ui"],
    reason: null,
    inherited_from: "CORE-1",
  });
  try {
    view.q.getByText("dispatch-server");
    view.q.getByText("web");
    const retired = view.q.getByText("retired: legacy-ui");
    expect(retired.getAttribute("title")).toBe(
      "legacy-ui is no longer in the project's architecture model"
    );
    const ancestor = view.q.getByRole("link", { name: "CORE-1" }) as HTMLAnchorElement;
    expect(ancestor.getAttribute("href")).toBe("/issues/CORE-1");
    expect(view.q.getByTestId("issue-components").textContent).toContain("inherited from CORE-1");
  } finally {
    view.unmount();
  }
});

test("IssueComponentsLine shows a none attachment with its reason", () => {
  const view = renderLine({
    mode: "none",
    ids: [],
    unknown: [],
    reason: "hiring, not code",
    inherited_from: null,
  });
  try {
    expect(view.q.getByTestId("issue-components").textContent).toBe(
      "Components:None — hiring, not code"
    );
    expect(view.q.queryByRole("link")).toBeNull();
  } finally {
    view.unmount();
  }
});

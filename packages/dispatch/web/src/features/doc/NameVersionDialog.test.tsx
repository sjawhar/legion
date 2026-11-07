import { expect, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { NameVersionDialog } from "./NameVersionDialog";

test("renders nothing when closed", () => {
  const view = render(
    <NameVersionDialog
      error={false}
      onClose={() => {}}
      onSave={() => {}}
      open={false}
      saving={false}
    />
  );
  try {
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});
test("Save is disabled until the input is non-empty", async () => {
  const user = userEvent.setup();
  const view = render(
    <NameVersionDialog error={false} onClose={() => {}} onSave={() => {}} open saving={false} />
  );
  try {
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    const input = screen.getByLabelText("What changed?");
    expect(save.disabled).toBe(true);

    await user.click(input);
    await user.paste("   ");
    expect(save.disabled).toBe(true);

    await user.clear(input);
    await user.click(input);
    await user.paste("Tightened the intro");
    expect(save.disabled).toBe(false);
  } finally {
    view.unmount();
  }
});

test("Enter submits the trimmed summary", async () => {
  const user = userEvent.setup();
  const saved: string[] = [];
  const view = render(
    <NameVersionDialog
      error={false}
      onClose={() => {}}
      onSave={(summary) => saved.push(summary)}
      open
      saving={false}
    />
  );
  try {
    const input = screen.getByLabelText("What changed?");
    await user.click(input);
    await user.paste("  Tightened the intro  ");
    await user.keyboard("{Enter}");
    expect(saved).toEqual(["Tightened the intro"]);
  } finally {
    view.unmount();
  }
});

test("Enter does nothing while the input is empty", async () => {
  const user = userEvent.setup();
  const saved: string[] = [];
  const view = render(
    <NameVersionDialog
      error={false}
      onClose={() => {}}
      onSave={(summary) => saved.push(summary)}
      open
      saving={false}
    />
  );
  try {
    const input = screen.getByLabelText("What changed?");
    await user.click(input);
    await user.keyboard("{Enter}");
    expect(saved).toEqual([]);
  } finally {
    view.unmount();
  }
});

test("Cancel calls onClose", async () => {
  const user = userEvent.setup();
  let closed = 0;
  const view = render(
    <NameVersionDialog
      error={false}
      onClose={() => (closed += 1)}
      onSave={() => {}}
      open
      saving={false}
    />
  );
  try {
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(closed).toBe(1);
  } finally {
    view.unmount();
  }
});

test("a failed save keeps the dialog and the typed summary and shows the error inside it", () => {
  const view = render(
    <NameVersionDialog error onClose={() => {}} onSave={() => {}} open saving={false} />
  );
  try {
    expect(within(view.container).getByRole("alert")).toHaveTextContent(
      "Could not name this version"
    );
    expect(
      (within(view.container).getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled
    ).toBe(true);
  } finally {
    view.unmount();
  }
});

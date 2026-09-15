import { expect, test } from "bun:test";
import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";

import { MultiSelect } from "./MultiSelect";

function Host({ onCreate, options }: { onCreate?: (value: string) => void; options: string[] }) {
  const [open, setOpen] = useState(false);
  const [selected, setSelected] = useState<string[]>([]);
  return (
    <>
      <MultiSelect
        emptyMessage="Nothing here."
        label="Labels"
        onChange={setSelected}
        onCreate={onCreate}
        onOpenChange={setOpen}
        open={open}
        options={options}
        searchLabel="Search labels"
        selected={selected}
      />
      <output data-testid="selected">{selected.join(",")}</output>
    </>
  );
}

test("clicking rows toggles a multi-selection, the trigger counts it and Clear empties it", () => {
  const view = render(<Host options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: "Labels" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(trigger);
    const listbox = screen.getByRole("listbox", { name: "Labels options" });
    expect(listbox.getAttribute("aria-multiselectable")).toBe("true");
    expect(document.activeElement).toBe(screen.getByRole("combobox", { name: "Search labels" }));

    fireEvent.click(screen.getByRole("option", { name: "docs" }));
    fireEvent.click(screen.getByRole("option", { name: "api" }));
    expect(screen.getByTestId("selected").textContent).toBe("docs,api");
    expect(screen.getByRole("option", { name: "docs" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("option", { name: "frontend" }).getAttribute("aria-selected")).toBe(
      "false"
    );
    expect(screen.getByRole("button", { name: "Labels · 2" }).getAttribute("aria-expanded")).toBe(
      "true"
    );

    fireEvent.click(screen.getByRole("option", { name: "docs" }));
    expect(screen.getByTestId("selected").textContent).toBe("api");

    fireEvent.click(screen.getByRole("button", { name: "Clear labels" }));
    expect(screen.getByTestId("selected").textContent).toBe("");
    expect(screen.queryByRole("button", { name: "Clear labels" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("the search narrows the rows and arrows, Enter and Escape drive them from the input", () => {
  const view = render(<Host options={["api", "docs", "frontend"]} />);
  try {
    const trigger = screen.getByRole("button", { name: "Labels" });
    fireEvent.click(trigger);
    const search = screen.getByRole("combobox", { name: "Search labels" });

    fireEvent.change(search, { target: { value: "DOC" } });
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual(["docs"]);
    fireEvent.change(search, { target: { value: "" } });

    // The first row is active until an arrow moves it; Enter toggles the active row.
    expect(search.getAttribute("aria-activedescendant")).toBe(
      screen.getByRole("option", { name: "api" }).id
    );
    fireEvent.keyDown(search, { key: "ArrowDown" });
    fireEvent.keyDown(search, { key: "ArrowDown" });
    expect(search.getAttribute("aria-activedescendant")).toBe(
      screen.getByRole("option", { name: "frontend" }).id
    );
    fireEvent.keyDown(search, { key: "Enter" });
    expect(screen.getByTestId("selected").textContent).toBe("frontend");
    // ArrowUp wraps from the first row to the last.
    fireEvent.keyDown(search, { key: "ArrowUp" });
    fireEvent.keyDown(search, { key: "ArrowUp" });
    fireEvent.keyDown(search, { key: "ArrowUp" });
    expect(search.getAttribute("aria-activedescendant")).toBe(
      screen.getByRole("option", { name: "frontend" }).id
    );

    fireEvent.change(search, { target: { value: "zzz" } });
    expect(screen.getByText("Nothing here.")).toBeTruthy();
    fireEvent.keyDown(search, { key: "Enter" });
    expect(screen.getByTestId("selected").textContent).toBe("frontend");

    fireEvent.keyDown(search, { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(trigger);
  } finally {
    view.unmount();
  }
});

test("with onCreate, a search that matches no option offers a Create row that Enter takes", () => {
  const created: string[] = [];
  const view = render(<Host onCreate={(value) => created.push(value)} options={["docs"]} />);
  try {
    fireEvent.click(screen.getByRole("button", { name: "Labels" }));
    const search = screen.getByRole("combobox", { name: "Search labels" });
    fireEvent.change(search, { target: { value: "Docs" } });
    expect(screen.queryByRole("option", { name: /Create/ })).toBeNull();
    fireEvent.change(search, { target: { value: " urgent " } });
    expect(screen.getByRole("option", { name: 'Create "urgent"' })).toBeTruthy();
    fireEvent.keyDown(search, { key: "ArrowDown" });
    fireEvent.keyDown(search, { key: "Enter" });
    expect(created).toEqual(["urgent"]);
    expect((search as HTMLInputElement).value).toBe("");
  } finally {
    view.unmount();
  }
});

test("an outside pointer press closes the popover", () => {
  const view = render(
    <>
      <Host options={["docs"]} />
      <button type="button">Elsewhere</button>
    </>
  );
  try {
    fireEvent.click(screen.getByRole("button", { name: "Labels" }));
    expect(screen.getByRole("listbox")).toBeTruthy();
    fireEvent.pointerDown(screen.getByRole("option", { name: "docs" }));
    expect(screen.getByRole("listbox")).toBeTruthy();
    fireEvent.pointerDown(screen.getByRole("button", { name: "Elsewhere" }));
    expect(screen.queryByRole("listbox")).toBeNull();
  } finally {
    view.unmount();
  }
});

test("optionLabel renders the row text and the search matches it while the value stays the key", () => {
  const changes: string[][] = [];
  const view = render(
    <MultiSelect
      emptyMessage="Nothing here."
      label="Status"
      onChange={(next) => changes.push(next)}
      onOpenChange={() => undefined}
      open
      optionLabel={(value) => (value === "in_progress" ? "In progress" : value)}
      options={["todo", "in_progress"]}
      searchLabel="Search statuses"
      selected={[]}
    />
  );
  try {
    const search = screen.getByRole("combobox", { name: "Search statuses" });
    fireEvent.change(search, { target: { value: "in prog" } });
    expect(screen.getAllByRole("option").map((option) => option.textContent)).toEqual([
      "In progress",
    ]);
    fireEvent.click(screen.getByRole("option", { name: "In progress" }));
    expect(changes).toEqual([["in_progress"]]);
  } finally {
    view.unmount();
  }
});

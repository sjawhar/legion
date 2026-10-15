import { expect, mock, test } from "bun:test";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { WindowControl } from "./WindowControl";

test("a preset calls back with that preset", () => {
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    fireEvent.change(screen.getByLabelText("Window"), { target: { value: "7d" } });
    expect(onChange).toHaveBeenCalledWith({ kind: "preset", preset: "7d" });
  } finally {
    cleanup();
  }
});

test("a custom range applies its whole UTC days", () => {
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    fireEvent.change(screen.getByLabelText("Window"), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-01-01" } });
    fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-01-07" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(onChange).toHaveBeenCalledWith({
      kind: "custom",
      from: "2026-01-01T00:00:00.000Z",
      to: "2026-01-08T00:00:00.000Z",
    });
  } finally {
    cleanup();
  }
});

test("a custom range over 84 days is refused with the limit stated, and nothing changes", () => {
  const onChange = mock();
  render(<WindowControl onChange={onChange} value={{ kind: "preset", preset: "28d" }} />);
  try {
    fireEvent.change(screen.getByLabelText("Window"), { target: { value: "custom" } });
    fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-01-01" } });
    fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-04-30" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    expect(screen.getByRole("alert").textContent).toBe(
      "The window can span at most 84 days; this range spans 120."
    );
    expect(onChange).not.toHaveBeenCalled();
  } finally {
    cleanup();
  }
});

test("a custom window from the URL shows its own dates", () => {
  render(
    <WindowControl
      onChange={() => {}}
      value={{ kind: "custom", from: "2026-01-01T00:00:00.000Z", to: "2026-01-08T00:00:00.000Z" }}
    />
  );
  try {
    expect((screen.getByLabelText("Window") as HTMLSelectElement).value).toBe("custom");
    expect((screen.getByLabelText("From") as HTMLInputElement).value).toBe("2026-01-01");
    expect((screen.getByLabelText("To") as HTMLInputElement).value).toBe("2026-01-07");
  } finally {
    cleanup();
  }
});

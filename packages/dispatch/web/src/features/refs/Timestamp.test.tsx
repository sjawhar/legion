import { expect, spyOn, test } from "bun:test";
import { render } from "@testing-library/react";

import { Timestamp } from "./Timestamp";

test("Timestamp renders the current relative label for a frozen clock", () => {
  const now = Date.parse("2026-09-10T12:00:00.000Z");
  const at = new Date(now - 5 * 60_000).toISOString();
  const nowSpy = spyOn(Date, "now").mockReturnValue(now);
  const view = render(<Timestamp at={at} />);

  try {
    const time = view.getByText("5 minutes ago");
    expect(time.tagName).toBe("TIME");
    expect(time.getAttribute("dateTime")).toBe(at);
    expect(time.getAttribute("title")).toBe(new Date(at).toLocaleString());
  } finally {
    view.unmount();
    nowSpy.mockRestore();
  }
});

test("Timestamp reflects a future time as 'in N minutes'", () => {
  const now = Date.parse("2026-09-10T12:00:00.000Z");
  const at = new Date(now + 2 * 60_000).toISOString();
  const nowSpy = spyOn(Date, "now").mockReturnValue(now);
  const view = render(<Timestamp at={at} />);

  try {
    expect(view.getByText("in 2 minutes")).toBeDefined();
  } finally {
    view.unmount();
    nowSpy.mockRestore();
  }
});

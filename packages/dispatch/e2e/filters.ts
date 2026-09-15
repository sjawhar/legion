import { expect, type Locator, type Page } from "@playwright/test";

/** The strip's `Labels` / `Status` picker trigger, whatever its selection count reads. */
export function filterPicker(page: Page, picker: "Labels" | "Status"): Locator {
  return page.getByRole("button", { name: new RegExp(`^${picker}( · \\d+)?$`) });
}

/** Toggles one option in an expanded strip's `Labels` or `Status` multi-select, then closes the
 *  popover with Escape so it never covers the control the test drives next. (Toggling the last
 *  active filter off collapses the strip, which already unmounts the popover.) */
export async function pickFilterOption(
  page: Page,
  picker: "Labels" | "Status",
  option: string
): Promise<void> {
  const trigger = filterPicker(page, picker);
  if ((await trigger.getAttribute("aria-expanded")) === "false") {
    await trigger.click();
  }
  await page.getByRole("option", { exact: true, name: option }).click();
  if ((await page.getByRole("listbox").count()) > 0) {
    await page.keyboard.press("Escape");
  }
  await expect(page.getByRole("listbox")).toHaveCount(0);
}

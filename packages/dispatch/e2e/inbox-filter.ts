import { expect, type Locator, type Page } from "@playwright/test";

/**
 * The Inbox's `?assignee=` control (`features/inbox/AssigneeFilter.tsx`), one of the five fields
 * the bar's **Filter** button opens (INFRA-450; before it, the control stood in the bar itself).
 * Driving it, rather than opening a filtered address, is what a test needs when the Inbox must
 * stay mounted across the change: a pick is a client-side `replace`, so the marks, drafts and
 * held responses the page is carrying survive it, where `page.goto` would lose them.
 */

/** The filter's trigger, whatever its selection reads (`Assignee · you`, `· everyone`, `· 2`);
 *  absent until the bar's **Filter** panel is open. */
export function assigneeFilter(page: Page): Locator {
  return page.getByRole("button", { name: "Filter by assignee" });
}

/** The popover's rows; absent while it is shut. */
export function assigneeOptions(page: Page): Locator {
  return page.getByRole("listbox", { name: "Assignee options" });
}

/** Opens the bar's five filter fields unless they are open already. */
export async function openFilterFields(page: Page): Promise<void> {
  const fields = page.locator("[data-inbox-filter-fields]");
  if ((await fields.count()) === 0) {
    await page.locator("[data-inbox-filter]").click();
  }
  await expect(fields).toHaveCount(1);
}

/** Opens the popover unless it is open already; the trigger says which it is. */
export async function openAssigneeFilter(page: Page): Promise<void> {
  await openFilterFields(page);
  const trigger = assigneeFilter(page);
  if ((await trigger.getAttribute("aria-expanded")) !== "true") {
    await trigger.click();
  }
  await expect(assigneeOptions(page)).toHaveCount(1);
}

/** Ticks one of the popover's rows - a login, or `Unassigned` - and leaves the popover open, as
 *  the control does. A row's accessible name carries its detail (`alice, you`), so `option` is
 *  matched as a substring of it. */
export async function pickAssignee(page: Page, option: string): Promise<void> {
  await openAssigneeFilter(page);
  await assigneeOptions(page).getByRole("option", { name: option }).click();
}

/** The popover's own `Clear assignee`: the cleared filter, every question. */
export async function clearAssigneeFilter(page: Page): Promise<void> {
  await openAssigneeFilter(page);
  await page.getByRole("button", { name: "Clear assignee" }).click();
}

/** Closes the popover the way a reader does, and waits until it is gone, so whatever it covered
 *  is reachable again. Escape is handled by the search box, which holds focus while the popover
 *  is open (a row's press never takes it). */
export async function closeAssigneeFilter(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(assigneeOptions(page)).toHaveCount(0);
}

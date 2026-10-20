import { expect, type Locator } from "@playwright/test";

/** Fails unless `pill` and `region` come to share no point on screen: the pill covers none of it.
 *  It waits for a layout that settles after a resize. */
export async function expectUncovered(pill: Locator, region: Locator): Promise<void> {
  await expect(async () => {
    const covering = await pill.boundingBox();
    const covered = await region.boundingBox();
    expect(covering).not.toBeNull();
    expect(covered).not.toBeNull();
    if (covering === null || covered === null) return;
    const apart =
      covering.x + covering.width <= covered.x ||
      covered.x + covered.width <= covering.x ||
      covering.y + covering.height <= covered.y ||
      covered.y + covered.height <= covering.y;
    expect(apart, `the pill at ${JSON.stringify(covering)} covers ${JSON.stringify(covered)}`).toBe(
      true
    );
  }).toPass({ timeout: 5_000 });
}

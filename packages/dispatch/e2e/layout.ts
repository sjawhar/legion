import { expect, type Locator } from "@playwright/test";

export interface Box {
  readonly height: number;
  readonly width: number;
  readonly x: number;
  readonly y: number;
}

/** Whether `a` and `b` share no point on screen; boxes that only touch at an edge are apart. */
export function boxesApart(a: Box, b: Box): boolean {
  return (
    a.x + a.width <= b.x || b.x + b.width <= a.x || a.y + a.height <= b.y || b.y + b.height <= a.y
  );
}

/** Fails unless `pill` and `region` come to share no point on screen: the pill covers none of it.
 *  It waits for a layout that settles after a resize. */
export async function expectUncovered(pill: Locator, region: Locator): Promise<void> {
  await expect(async () => {
    const covering = await pill.boundingBox();
    const covered = await region.boundingBox();
    expect(covering).not.toBeNull();
    expect(covered).not.toBeNull();
    if (covering === null || covered === null) return;
    const apart = boxesApart(covering, covered);
    expect(apart, `the pill at ${JSON.stringify(covering)} covers ${JSON.stringify(covered)}`).toBe(
      true
    );
  }).toPass({ timeout: 5_000 });
}

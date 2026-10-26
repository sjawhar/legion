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

/** Two links on consecutive lines of one body: how far the lower link's padded box starts below
 *  the upper link's own text (negative: padding over letters a reader taps), with the text
 *  metrics that say which pitch was measured. */
export async function consecutiveLinkClearance(
  scope: Locator,
  upper: string,
  lower: string
): Promise<{ clearance: number; linePitch: number; lineHeight: number; fontSize: number }> {
  // One synchronous read: the geometry is relative, so a scroll between reads cannot skew it.
  return scope.evaluate(
    (element, names) => {
      const links = [...element.querySelectorAll("a")];
      const named = (text: string) => {
        const link = links.find((candidate) => (candidate.textContent ?? "") === text);
        if (link === undefined) throw new Error(`no link named "${text}" in the scope`);
        return link;
      };
      const upperLink = named(names.upper);
      const lowerLink = named(names.lower);
      const range = document.createRange();
      range.selectNodeContents(upperLink);
      const upperText = range.getBoundingClientRect();
      range.selectNodeContents(lowerLink);
      const lowerText = range.getBoundingClientRect();
      const style = getComputedStyle(upperLink);
      return {
        clearance: lowerLink.getBoundingClientRect().top - upperText.bottom,
        fontSize: Number.parseFloat(style.fontSize),
        lineHeight: Number.parseFloat(style.lineHeight),
        linePitch: lowerText.top - upperText.top,
      };
    },
    { lower, upper }
  );
}

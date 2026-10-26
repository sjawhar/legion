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

/** Fails unless two links on consecutive lines of one body, set in `pitch`, keep the lower link's
 *  padded box off the upper link's own text: a padded box over those letters takes a tap a
 *  reader meant for the upper line. The text size, line height and line pitch are checked first,
 *  so the clearance is known to be the one at that pitch. */
export async function expectConsecutiveLinksClear(
  scope: Locator,
  upper: string,
  lower: string,
  pitch: { fontSize: number; lineHeight: number }
): Promise<void> {
  // One synchronous read: the geometry is relative, so a scroll between reads cannot skew it.
  const measured = await scope.evaluate(
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
  expect(
    Math.abs(measured.fontSize - pitch.fontSize),
    `"${upper}" is set at ${measured.fontSize}px, not ${pitch.fontSize}px, so this measures another pitch`
  ).toBeLessThanOrEqual(0.1);
  expect(
    Math.abs(measured.lineHeight - pitch.lineHeight),
    `"${upper}" sits on ${measured.lineHeight}px lines, not ${pitch.lineHeight}px, so this measures another pitch`
  ).toBeLessThanOrEqual(0.1);
  expect(
    Math.abs(measured.linePitch - measured.lineHeight),
    `"${upper}" and "${lower}" are ${measured.linePitch}px apart, not one ${measured.lineHeight}px line, so they are not on consecutive lines`
  ).toBeLessThanOrEqual(0.5);
  expect(
    measured.clearance,
    `the padded box of "${lower}" reaches above the bottom of the text of "${upper}", so a tap on the upper line can open the lower link`
  ).toBeGreaterThanOrEqual(0);
}

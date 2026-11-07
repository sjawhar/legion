import {
  activeTabIndicatorBorder,
  activeTabIndicatorText,
  borderDefault,
  textSecondaryOnCanvas,
} from "../theme/classes";

/**
 * What a tab strip is made of, in one place, because two surfaces draw the same strip: `Tabs`
 * itself, and the box the issue page keeps for it while its tabs are still to come
 * (`features/issue/issue-page-skeleton.tsx`, dispatch://INFRA-341 R11). A placeholder drawn at
 * the strip's size is that size only while both read the same classes; a copy of them drifts
 * the first time one changes, and the strip then moves the document under the reader as it
 * lands.
 *
 * Compact tabs sit tighter on phones, which is what keeps the shorter tablists on one line.
 */

/** The strip itself: the row the tablist and anything trailing it sit in. */
export function tabStripClassName(compact: boolean): string {
  return `${compact ? "mb-0" : "mb-3"} flex flex-wrap items-center gap-1 border-b ${borderDefault}`;
}

/** The tablist inside it, which scrolls sideways where the tabs are wider than the page. */
export function tabListClassName(compact: boolean): string {
  return `flex min-w-0 ${compact ? "gap-0 md:gap-1" : "gap-1"} overflow-x-auto [scrollbar-width:thin]`;
}

/** One tab. The selected one carries a 2 px underline and a heavier weight, neither of which
 *  changes its height: the minimum is 44 px and the content under it stands at 38 px. */
export function tabClassName(compact: boolean, selected: boolean): string {
  const minHeight = compact ? "min-h-11 md:min-h-9" : "min-h-11";
  const padding = compact ? "px-1.5 md:px-2" : "px-3";
  const box = `${minHeight} ${padding} inline-flex shrink-0 items-center gap-1.5 py-2 text-sm whitespace-nowrap`;
  return selected
    ? `${box} border-b-2 font-semibold ${activeTabIndicatorBorder} ${activeTabIndicatorText}`
    : `${box} ${textSecondaryOnCanvas}`;
}

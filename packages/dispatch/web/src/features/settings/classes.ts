import {
  borderDefault,
  card,
  inputClasses,
  primaryButtonBg,
  primaryButtonHoverBg,
  surfaceMutedBg,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";

/** The recipes every Settings section's table and form share, so the sections stay in step. */
/** `relative` is load-bearing: a table's `sr-only` header cell is absolutely positioned, and
 *  without a positioned ancestor inside the scroll container its containing block is the page,
 *  so it sits at the scrolled table's own x and widens the document past the viewport (the
 *  Settings page measured 417px at 390px). The table itself already scrolls here. */
export const settingsTableWrapper = `relative mt-6 overflow-x-auto rounded-xl border ${card}`;
export const settingsTableHead = `border-b ${surfaceMutedBg} ${borderDefault} ${textSecondaryOnSurface}`;
export const settingsTableRow = `border-b last:border-0 ${borderDefault}`;
export const settingsFieldLabel = `grid gap-1 text-sm font-medium ${textSecondaryOnCanvas}`;
export const settingsMonoInput = `rounded-md px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnSurface}`;
export const settingsSubmitButton = `min-h-11 rounded-md px-4 py-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 md:min-h-9 ${primaryButtonBg} ${primaryButtonHoverBg}`;

/** The gap between one Settings section and the next: the spec's step between the parts of a
 *  page. The page owns the rhythm - no section sets an outer margin of its own, which is how
 *  three sections came to line up at 40px by coincidence rather than by rule. */
export const settingsSectionGap = "space-y-4";

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
export const settingsTableWrapper = `mt-6 overflow-x-auto rounded-xl border ${card}`;
export const settingsTableHead = `border-b ${surfaceMutedBg} ${borderDefault} ${textSecondaryOnSurface}`;
export const settingsTableRow = `border-b last:border-0 ${borderDefault}`;
export const settingsFieldLabel = `grid gap-1 text-sm font-medium ${textSecondaryOnCanvas}`;
export const settingsMonoInput = `rounded-md px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnSurface}`;
export const settingsSubmitButton = `rounded-md px-4 py-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 ${primaryButtonBg} ${primaryButtonHoverBg}`;

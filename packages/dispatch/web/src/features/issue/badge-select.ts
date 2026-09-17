import { hasFocusVisibleRing } from "../../theme/classes";

/** The "tinted badge with an invisible native select over it" control (priority, assignee):
 *  the wrapper is the 44 px tap target (32 px from `md`) and draws the keyboard focus ring. */
export const badgeSelectWrapper = `relative inline-flex min-h-11 shrink-0 items-center rounded-sm md:min-h-8 has-focus-visible:ring-2 ${hasFocusVisibleRing}`;
/** The visible badge; the caller appends its tone's `bg` and `text`. */
export const badgeSelectBadge =
  "inline-flex items-center rounded-sm px-1.5 py-0.5 text-xs font-semibold";
/** The native select laid over the badge, invisible but the thing that receives the pick. */
export const badgeSelectOverlay =
  "absolute inset-0 size-full cursor-pointer appearance-none opacity-0 disabled:cursor-not-allowed";

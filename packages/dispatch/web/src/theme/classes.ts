import type { Swatch } from "./palette";
import * as P from "./palette";

/**
 * The only place a component-facing `dark:`-paired className string gets built. Every exported
 * class string below is a **static literal** — Tailwind's build-time scanner (`@tailwindcss/
 * vite`) finds which utility classes to generate by reading source files as plain text; it does
 * not evaluate JavaScript, so a class name assembled at runtime by concatenating a prefix
 * variable with a swatch-name variable never appears as literal text anywhere, and Tailwind
 * silently omits it from the built stylesheet. `classes-in-build-css.test.ts` guards this by
 * building the project and checking every token below exists in the output CSS;
 * `no-raw-colors.test.ts` additionally rejects a hyphen glued directly onto a template
 * expression in this very file, so the bug that motivated both tests cannot come back.
 *
 * Nothing outside `theme/` should write a `bg-`/`text-`/`border-`/`ring-`/`placeholder-` color
 * literal directly — `no-raw-colors.test.ts` greps `web/src` (excluding `theme/`) for exactly
 * that and fails on any hit, so a new component cannot bypass this module and a re-color has one
 * place to change.
 *
 * `CONTRAST_CHECKS` is built up alongside each role that pairs text against a background, using
 * the OKLCH swatches from `palette.ts` (contrast math has no Tailwind-scanning constraint — it
 * never touches a className). Keeping each literal string and its OKLCH pair next to each other
 * is what keeps them from drifting apart.
 */

export interface ColorPair {
  readonly light: Swatch;
  readonly dark: Swatch;
}

function pair(light: Swatch, dark: Swatch): ColorPair {
  return { dark, light };
}

export interface ContrastCheck {
  readonly description: string;
  readonly foreground: ColorPair;
  readonly background: ColorPair;
  /** WCAG 2 AA: 4.5:1 for normal text, 3:1 for large text (>=24px, or >=19px bold) and for the
   * boundary of a focus indicator against its background. */
  readonly minRatio: 4.5 | 3;
}

export const CONTRAST_CHECKS: ContrastCheck[] = [];

function registerText(
  description: string,
  foreground: ColorPair,
  background: ColorPair,
  minRatio: 4.5 | 3 = 4.5
): void {
  CONTRAST_CHECKS.push({ background, description, foreground, minRatio });
}

// ---------------------------------------------------------------------------------------------
// Canvas and surfaces
// ---------------------------------------------------------------------------------------------

export const CANVAS = pair(P.SLATE_50, P.SLATE_950);
export const SURFACE = pair(P.WHITE, P.SLATE_900);
export const SURFACE_MUTED = pair(P.SLATE_100, P.SLATE_800);
export const SURFACE_MUTED_STRONG = pair(P.SLATE_200, P.SLATE_700);
/** An input recessed inside a `SURFACE` card needs to read as a level below it. */
export const SURFACE_RECESSED = pair(P.WHITE, P.SLATE_950);

export const canvasBg = "bg-slate-50 dark:bg-slate-950";
export const surfaceBg = "bg-white dark:bg-slate-900";
export const surfaceMutedBg = "bg-slate-100 dark:bg-slate-800";
export const surfaceMutedHoverBg = "hover:bg-slate-100 dark:hover:bg-slate-800";
export const surfaceMutedStrongBg = "bg-slate-200 dark:bg-slate-700";
export const surfaceRecessedBg = "bg-white dark:bg-slate-950";
/** An `animate-pulse` skeleton placeholder: `SURFACE_MUTED`'s shades reused directly (both
 * `bg-slate-100` and `bg-slate-200` skeletons in the app already resolve to the same
 * `dark:bg-slate-800`; this consolidates the light side to one shade). */
export const skeletonBg = surfaceMutedBg;

// ---------------------------------------------------------------------------------------------
// Text
// ---------------------------------------------------------------------------------------------

export const TEXT_PRIMARY = pair(P.SLATE_900, P.SLATE_100);
export const TEXT_SECONDARY = pair(P.SLATE_700, P.SLATE_300);
export const TEXT_MUTED = pair(P.SLATE_500, P.SLATE_400);
/** Disabled text is WCAG-exempt (inactive controls, SC 1.4.11) — deliberately not registered. */
export const TEXT_DISABLED = pair(P.SLATE_400, P.SLATE_600);
/** `TEXT_MUTED` (slate-500) measures 4.35:1 on `SURFACE_MUTED`'s light shade (slate-100) — under
 * AA. This pairing needs its own, slightly stronger light shade to actually clear 4.5:1. */
export const TEXT_MUTED_ON_SURFACE_MUTED = pair(P.SLATE_600, P.SLATE_400);

export const textPrimaryOnCanvas = "text-slate-900 dark:text-slate-100";
export const textPrimaryOnSurface = "text-slate-900 dark:text-slate-100";
export const textSecondaryOnCanvas = "text-slate-700 dark:text-slate-300";
export const textSecondaryOnSurface = "text-slate-700 dark:text-slate-300";
export const textMutedOnCanvas = "text-slate-500 dark:text-slate-400";
export const textMutedOnSurface = "text-slate-500 dark:text-slate-400";
export const textPrimaryOnSurfaceMuted = "text-slate-900 dark:text-slate-100";
export const textSecondaryOnSurfaceMuted = "text-slate-700 dark:text-slate-300";
export const textMutedOnSurfaceMuted = "text-slate-600 dark:text-slate-400";
export const textMutedOnSelectedCard = "text-slate-600 dark:text-slate-400";
export const textDisabled = "text-slate-400 dark:text-slate-600";
/** A dialog/composer "Close" button: secondary at rest, primary-emphasis on hover. Not
 * registered separately — both ends reuse `TEXT_SECONDARY`/`TEXT_PRIMARY`'s own checks. */
export const dismissButtonText =
  "text-slate-700 dark:text-slate-300 hover:text-slate-900 dark:hover:text-slate-100";
/** A disclosure chevron: disabled-shade at rest, secondary-emphasis on hover. */
export const disclosureButtonText =
  "text-slate-400 dark:text-slate-600 hover:text-slate-700 dark:hover:text-slate-300";
/** A "dismiss"/cancel affordance already showing muted text: secondary-emphasis on hover. */
export const textMutedHoverToSecondary =
  "text-slate-500 dark:text-slate-400 hover:text-slate-700 dark:hover:text-slate-300";
/** A toggle already showing secondary text: primary-emphasis on hover (e.g. an editable field's
 * placeholder-affordance button). */
export const textSecondaryHoverToPrimary =
  "text-slate-700 dark:text-slate-300 hover:text-slate-900 dark:hover:text-slate-100";
/** A muted affordance that jumps straight to primary-emphasis on hover. */
export const textMutedHoverToPrimary =
  "text-slate-500 dark:text-slate-400 hover:text-slate-900 dark:hover:text-slate-100";
/** `SURFACE_MUTED` used as a disabled-state background (e.g. a `<select>` that greys out rather
 * than just dimming its text). */
export const surfaceMutedDisabledBg = "disabled:bg-slate-100 dark:disabled:bg-slate-800";
export const kbdHint = `${surfaceMutedBg} ${textMutedOnSurfaceMuted}`;

registerText("primary text on canvas", TEXT_PRIMARY, CANVAS);
registerText("primary text on surface", TEXT_PRIMARY, SURFACE);
registerText("secondary text on canvas", TEXT_SECONDARY, CANVAS);
registerText("secondary text on surface", TEXT_SECONDARY, SURFACE);
registerText("muted text on canvas", TEXT_MUTED, CANVAS);
registerText("muted text on surface", TEXT_MUTED, SURFACE);
registerText("primary text on surface-muted", TEXT_PRIMARY, SURFACE_MUTED);
registerText("secondary text on surface-muted", TEXT_SECONDARY, SURFACE_MUTED);
registerText("muted text on surface-muted", TEXT_MUTED_ON_SURFACE_MUTED, SURFACE_MUTED);
/** A card whose background switches between `SURFACE` (resting) and a stronger-tinted
 * background (selected/highlighted) reuses this stronger muted shade for both, rather than
 * conditionally swapping the text role with the background. */
registerText("muted text (surface-muted shade) on surface", TEXT_MUTED_ON_SURFACE_MUTED, SURFACE);

// ---------------------------------------------------------------------------------------------
// Borders and focus
// ---------------------------------------------------------------------------------------------

export const BORDER_DEFAULT = pair(P.SLATE_200, P.SLATE_800);
export const BORDER_STRONG = pair(P.SLATE_300, P.SLATE_700);
/** The one border-only "identify this state" case in the app: a focus indicator. Registered at
 * 3:1 against both `CANVAS` and `SURFACE`, since a focusable control can sit on either. */
export const FOCUS_ACCENT = pair(P.SKY_600, P.SKY_400);
/** A card or row's hover affordance — decorative, not a focus indicator; not registered. */
export const CARD_HOVER_ACCENT = pair(P.SKY_400, P.SKY_300);
/** An outline button or input's hover affordance — decorative; not registered. */
export const CONTROL_HOVER_ACCENT = pair(P.SKY_500, P.SKY_400);

export const borderDefault = "border-slate-200 dark:border-slate-800";
export const borderStrong = "border-slate-300 dark:border-slate-700";
/** A plain (non-accent) hover affordance on an editable field that otherwise has no border,
 * e.g. an inline title showing its editability on hover without a persistent outline. */
export const borderStrongHover = "hover:border-slate-300 dark:hover:border-slate-700";
export const focusBorder = "focus:border-sky-600 dark:focus:border-sky-400";
export const focusRing = "focus:ring-sky-600 dark:focus:ring-sky-400";
export const focusVisibleRing = "focus-visible:ring-sky-600 dark:focus-visible:ring-sky-400";
export const cardHoverBorder = "hover:border-sky-400 dark:hover:border-sky-300";
/** The `cardHoverBorder` accent, scoped to `enabled:` so it doesn't apply while a button is
 * `disabled` (paired with `disabled:opacity-50`-style buttons). */
export const enabledCardHoverBorder =
  "enabled:hover:border-sky-400 dark:enabled:hover:border-sky-300";
export const controlHoverBorder = "hover:border-sky-500 dark:hover:border-sky-400";

registerText("focus ring on canvas", FOCUS_ACCENT, CANVAS, 3);
registerText("focus ring on surface", FOCUS_ACCENT, SURFACE, 3);

// ---------------------------------------------------------------------------------------------
// Links, danger, success
// ---------------------------------------------------------------------------------------------

export const LINK = pair(P.SKY_700, P.SKY_400);
export const LINK_HOVER = pair(P.SKY_900, P.SKY_300);
export const DANGER_TEXT = pair(P.ROSE_700, P.ROSE_400);
export const DANGER_HOVER_TEXT = pair(P.ROSE_900, P.ROSE_300);
export const DANGER_HOVER_BORDER = pair(P.ROSE_300, P.ROSE_800);
export const SUCCESS_TEXT = pair(P.EMERALD_700, P.EMERALD_400);
export const SUCCESS_HOVER_TEXT = pair(P.EMERALD_900, P.EMERALD_300);

export const linkText = "text-sky-700 dark:text-sky-400";
export const linkHoverText = "hover:text-sky-900 dark:hover:text-sky-300";
export const dangerText = "text-rose-700 dark:text-rose-400";
export const dangerHoverText = "hover:text-rose-900 dark:hover:text-rose-300";
export const dangerHoverBorder = "hover:border-rose-300 dark:hover:border-rose-800";
export const successText = "text-emerald-700 dark:text-emerald-400";
export const successHoverText = "hover:text-emerald-900 dark:hover:text-emerald-300";
/** A secondary/outline control that jumps straight to `DANGER_TEXT`'s own shades on hover
 * (stronger than `dangerHoverText`'s emphasis tier, e.g. "remove this row"). */
export const hoverToDangerText = "hover:text-rose-700 dark:hover:text-rose-400";

registerText("link on surface", LINK, SURFACE);
registerText("link hover on surface", LINK_HOVER, SURFACE);
registerText("link on canvas", LINK, CANVAS);
registerText("link hover on canvas", LINK_HOVER, CANVAS);
registerText("danger text on surface", DANGER_TEXT, SURFACE);
registerText("danger text on canvas", DANGER_TEXT, CANVAS);
registerText("success text on surface", SUCCESS_TEXT, SURFACE);
registerText("success text on canvas", SUCCESS_TEXT, CANVAS);
/** `linkText`, unqualified, also renders directly inside a `SURFACE_MUTED`/`SURFACE_RECESSED`
 * container in several places (an unfurled reference card, a Conversation turn's recessed quote
 * box) — `LINK` is a single fixed pair, so these are the same values already checked above, just
 * against a background that wasn't yet registered. */
registerText("link on surface-muted", LINK, SURFACE_MUTED);
registerText("link on surface-recessed", LINK, SURFACE_RECESSED);
registerText("secondary text on surface-recessed", TEXT_SECONDARY, SURFACE_RECESSED);

// ---------------------------------------------------------------------------------------------
// Callouts (border + background + title/body text triplets)
// ---------------------------------------------------------------------------------------------

export const CALLOUT_DANGER_BORDER = pair(P.ROSE_200, P.ROSE_900);
export const CALLOUT_DANGER_BG = pair(P.ROSE_50, P.ROSE_950);
export const CALLOUT_DANGER_TEXT = pair(P.ROSE_950, P.ROSE_300);
export const CALLOUT_WARNING_BORDER = pair(P.AMBER_200, P.AMBER_900);
export const CALLOUT_WARNING_BG = pair(P.AMBER_50, P.AMBER_950);
export const CALLOUT_WARNING_TEXT = pair(P.AMBER_950, P.AMBER_300);
export const CALLOUT_SUCCESS_BORDER = pair(P.EMERALD_200, P.EMERALD_900);
export const CALLOUT_SUCCESS_BG = pair(P.EMERALD_50, P.EMERALD_950);
export const CALLOUT_SUCCESS_TEXT = pair(P.EMERALD_900, P.EMERALD_300);
export const CALLOUT_INFO_BORDER = pair(P.SKY_200, P.SKY_900);
export const CALLOUT_INFO_BG = pair(P.SKY_50, P.SKY_950);
export const CALLOUT_INFO_TITLE_TEXT = pair(P.SKY_950, P.SKY_300);
/** Standalone warning/orphaned-anchor text that isn't inside a bordered callout box. */
export const INLINE_WARNING_TEXT = pair(P.AMBER_800, P.AMBER_300);

export const calloutDangerBorder = "border-rose-200 dark:border-rose-900";
export const calloutDangerBg = "bg-rose-50 dark:bg-rose-950";
export const calloutDangerText = "text-rose-950 dark:text-rose-300";
export const calloutWarningBorder = "border-amber-200 dark:border-amber-900";
export const calloutWarningBg = "bg-amber-50 dark:bg-amber-950";
export const calloutWarningText = "text-amber-950 dark:text-amber-300";
export const calloutSuccessBorder = "border-emerald-200 dark:border-emerald-900";
export const calloutSuccessBg = "bg-emerald-50 dark:bg-emerald-950";
export const calloutSuccessText = "text-emerald-900 dark:text-emerald-300";
/** The answered ask's quoted anchor and "who answered" timestamp inside the success callout —
 * `SUGGESTION_ADDED_TEXT`'s exact shades, reused here under their own name because this is a
 * different semantic role (callout body text, not a suggestion diff) even though the values
 * happen to coincide. */
export const CALLOUT_SUCCESS_BODY_TEXT = pair(P.EMERALD_800, P.EMERALD_300);
export const CALLOUT_SUCCESS_TIMESTAMP_TEXT = pair(P.EMERALD_700, P.EMERALD_400);
export const calloutSuccessBodyText = "text-emerald-800 dark:text-emerald-300";
export const calloutSuccessTimestampText = "text-emerald-700 dark:text-emerald-400";
/** A blockquote's left-border accent stripe: purely decorative (2px, no text), so it stays one
 * fixed shade in both schemes rather than a `dark:`-paired role. */
export const quoteAccentBorder = "border-sky-400";
export const successQuoteAccentBorder = "border-emerald-400";
/** A quoted anchor's body text (the quote itself, not the callout around it) — a recurring
 * pair distinct from `TEXT_SECONDARY`/`TEXT_MUTED` (AskCard, CommentsTab, and Composer all use
 * it verbatim for their anchor blockquotes). */
export const QUOTE_BODY_TEXT = pair(P.SLATE_600, P.SLATE_400);
export const quoteBodyText = "text-slate-600 dark:text-slate-400";
/** A selected/active card's accent border and background — `CALLOUT_INFO_BG`'s shades reused
 * for the fill, with a slightly bolder sky-500/400 border than `FOCUS_ACCENT` uses for a focus
 * ring (this is a resting selected state, not a focus indicator). */
export const SELECTED_CARD_BORDER = pair(P.SKY_500, P.SKY_400);
export const SELECTED_CARD_BG = pair(P.SKY_50, P.SKY_950);
/** Option descriptions need a stronger muted role to clear AA on selected sky cards. */
export const TEXT_OPTION_DESCRIPTION = pair(P.SLATE_600, P.SLATE_400);
export const selectedCardBorder = "border-sky-500 dark:border-sky-400";
export const selectedCardBg = "bg-sky-50 dark:bg-sky-950";
export const textOptionDescription = "text-slate-600 dark:text-slate-400";
/** A native checkbox's `accent-color` fill: fixed, not paired — accent-color has no reliable
 * `dark:` variant support across browsers, and sky-600 already reads fine on either scheme's
 * checkbox chrome. */
export const checkboxAccent = "text-sky-600";
/** A drag handle's grip bar: `BORDER_STRONG`'s shades used as a background fill rather than a
 * border — purely decorative, no contrast check applies. */
export const dragHandleBg = "bg-slate-300 dark:bg-slate-700";
/** A high-emphasis line (e.g. the original question) inside a success callout — `TEXT_PRIMARY`
 * rendered on `CALLOUT_SUCCESS_BG` rather than on `CANVAS`/`SURFACE`. */
export const textPrimaryOnSuccessCallout = "text-slate-900 dark:text-slate-100";
export const calloutInfoBorder = "border-sky-200 dark:border-sky-900";
export const calloutInfoBg = "bg-sky-50 dark:bg-sky-950";
export const calloutInfoHoverBg = "hover:bg-sky-50 dark:hover:bg-sky-950";
export const calloutInfoTitleText = "text-sky-950 dark:text-sky-300";
export const inlineWarningText = "text-amber-800 dark:text-amber-300";

registerText("danger callout text", CALLOUT_DANGER_TEXT, CALLOUT_DANGER_BG);
registerText("warning callout text", CALLOUT_WARNING_TEXT, CALLOUT_WARNING_BG);
registerText("success callout text", CALLOUT_SUCCESS_TEXT, CALLOUT_SUCCESS_BG);
registerText("primary text on success callout", TEXT_PRIMARY, CALLOUT_SUCCESS_BG);
registerText("success callout body text", CALLOUT_SUCCESS_BODY_TEXT, CALLOUT_SUCCESS_BG);
registerText("success callout timestamp text", CALLOUT_SUCCESS_TIMESTAMP_TEXT, CALLOUT_SUCCESS_BG);
registerText("quote body text on surface", QUOTE_BODY_TEXT, SURFACE);
registerText("info callout title text", CALLOUT_INFO_TITLE_TEXT, CALLOUT_INFO_BG);
registerText("inline warning text on surface", INLINE_WARNING_TEXT, SURFACE);
/** `CALLOUT_INFO_BG` also backs the composer form and a selected margin card (`selectedCardBg`
 * reuses its exact shades under its own name), so primary, secondary, and stronger muted text
 * roles need contrast checks.
 */
registerText("primary text on callout-info", TEXT_PRIMARY, CALLOUT_INFO_BG);
registerText("secondary text on callout-info", TEXT_SECONDARY, CALLOUT_INFO_BG);
registerText(
  "muted text (surface-muted shade) on callout-info",
  TEXT_MUTED_ON_SURFACE_MUTED,
  CALLOUT_INFO_BG
);
registerText("quote body text on callout-info", QUOTE_BODY_TEXT, CALLOUT_INFO_BG);
registerText("inline warning text on callout-info", INLINE_WARNING_TEXT, CALLOUT_INFO_BG);
registerText("success text on callout-info", SUCCESS_TEXT, CALLOUT_INFO_BG);
registerText("danger text on callout-info", DANGER_TEXT, CALLOUT_INFO_BG);
registerText("link on callout-info", LINK, CALLOUT_INFO_BG);
registerText("primary text on selected card", TEXT_PRIMARY, SELECTED_CARD_BG);
registerText("option description text on selected card", TEXT_OPTION_DESCRIPTION, SELECTED_CARD_BG);
registerText("link text on selected card", LINK, SELECTED_CARD_BG);

// ---------------------------------------------------------------------------------------------
// Suggestion and version diffs
// ---------------------------------------------------------------------------------------------

export const SUGGESTION_ADDED_BG = pair(P.EMERALD_50, P.EMERALD_950);
export const SUGGESTION_ADDED_TEXT = pair(P.EMERALD_800, P.EMERALD_300);
export const SUGGESTION_REMOVED_BG = pair(P.ROSE_50, P.ROSE_950);
export const SUGGESTION_REMOVED_TEXT = pair(P.ROSE_800, P.ROSE_300);
export const DIFF_ADDED_BG = pair(P.EMERALD_100, P.EMERALD_900);
export const DIFF_ADDED_TEXT = pair(P.EMERALD_950, P.EMERALD_100);
export const DIFF_REMOVED_BG = pair(P.ROSE_100, P.ROSE_900);
export const DIFF_REMOVED_TEXT = pair(P.ROSE_950, P.ROSE_100);

export const suggestionAddedBg = "bg-emerald-50 dark:bg-emerald-950";
export const suggestionAddedText = "text-emerald-800 dark:text-emerald-300";
export const suggestionRemovedBg = "bg-rose-50 dark:bg-rose-950";
export const suggestionRemovedText = "text-rose-800 dark:text-rose-300";
export const diffAddedBg = "bg-emerald-100 dark:bg-emerald-900";
export const diffAddedText = "text-emerald-950 dark:text-emerald-100";
export const diffRemovedBg = "bg-rose-100 dark:bg-rose-900";
export const diffRemovedText = "text-rose-950 dark:text-rose-100";

registerText("suggestion-added text", SUGGESTION_ADDED_TEXT, SUGGESTION_ADDED_BG);
registerText("suggestion-removed text", SUGGESTION_REMOVED_TEXT, SUGGESTION_REMOVED_BG);
registerText("diff-added text", DIFF_ADDED_TEXT, DIFF_ADDED_BG);
registerText("diff-removed text", DIFF_REMOVED_TEXT, DIFF_REMOVED_BG);

export const SEARCH_HIT_BG = pair(P.AMBER_100, P.AMBER_950);
export const SEARCH_HIT_TEXT = pair(P.AMBER_950, P.AMBER_100);
export const searchHitBg = "bg-amber-100 dark:bg-amber-950";
export const searchHitText = "text-amber-950 dark:text-amber-100";

registerText("search hit text", SEARCH_HIT_TEXT, SEARCH_HIT_BG);

// ---------------------------------------------------------------------------------------------
// Badges (urgency, primary-artifact, connection status)
// ---------------------------------------------------------------------------------------------

export const BADGE_BLOCKING_BG = pair(P.ROSE_100, P.ROSE_950);
export const BADGE_BLOCKING_TEXT = pair(P.ROSE_800, P.ROSE_300);
export const BADGE_HIGH_BG = pair(P.AMBER_100, P.AMBER_950);
export const BADGE_HIGH_TEXT = pair(P.AMBER_800, P.AMBER_300);
export const BADGE_MED_BG = pair(P.SKY_100, P.SKY_950);
export const BADGE_MED_TEXT = pair(P.SKY_800, P.SKY_300);
export const BADGE_LOW_BG = SURFACE_MUTED;
export const BADGE_LOW_TEXT = TEXT_SECONDARY;
export const BADGE_PRIMARY_BG = pair(P.SKY_100, P.SKY_950);
export const BADGE_PRIMARY_TEXT = pair(P.SKY_800, P.SKY_300);

export const badgeBlocking = {
  bg: "bg-rose-100 dark:bg-rose-950",
  text: "text-rose-800 dark:text-rose-300",
};
export const badgeHigh = {
  bg: "bg-amber-100 dark:bg-amber-950",
  text: "text-amber-800 dark:text-amber-300",
};
export const badgeMed = {
  bg: "bg-sky-100 dark:bg-sky-950",
  text: "text-sky-800 dark:text-sky-300",
};
export const badgeLow = {
  bg: "bg-slate-100 dark:bg-slate-800",
  text: "text-slate-700 dark:text-slate-300",
};
export const badgePrimary = {
  bg: "bg-sky-100 dark:bg-sky-950",
  text: "text-sky-800 dark:text-sky-300",
};

registerText("urgency:blocking badge", BADGE_BLOCKING_TEXT, BADGE_BLOCKING_BG);
registerText("urgency:high badge", BADGE_HIGH_TEXT, BADGE_HIGH_BG);
registerText("urgency:med badge", BADGE_MED_TEXT, BADGE_MED_BG);
registerText("urgency:low badge", BADGE_LOW_TEXT, BADGE_LOW_BG);
registerText("primary-artifact badge", BADGE_PRIMARY_TEXT, BADGE_PRIMARY_BG);
/** `badgeMed.text` (identical to `BADGE_MED_TEXT`) also renders standalone, without its own
 * `badgeMed.bg`, on top of an ambient `SURFACE`/`SURFACE_RECESSED` container - a reference chip
 * and a selection-menu action button both do this (`referencePillBorder`'s doc comment already
 * notes the reuse). */
registerText("badge text on surface", BADGE_MED_TEXT, SURFACE);
registerText("badge text on surface-recessed", BADGE_MED_TEXT, SURFACE_RECESSED);

export const STATUS_CONNECTED_BG = pair(P.EMERALD_100, P.EMERALD_950);
export const STATUS_CONNECTED_TEXT = pair(P.EMERALD_800, P.EMERALD_300);
export const STATUS_CONNECTING_BG = pair(P.AMBER_100, P.AMBER_950);
export const STATUS_CONNECTING_TEXT = pair(P.AMBER_800, P.AMBER_300);
export const STATUS_OFFLINE_BG = SURFACE_MUTED_STRONG;
export const STATUS_OFFLINE_TEXT = TEXT_SECONDARY;

export const statusConnected = {
  bg: "bg-emerald-100 dark:bg-emerald-950",
  text: "text-emerald-800 dark:text-emerald-300",
};
export const statusConnecting = {
  bg: "bg-amber-100 dark:bg-amber-950",
  text: "text-amber-800 dark:text-amber-300",
};
export const statusOffline = {
  bg: "bg-slate-200 dark:bg-slate-700",
  text: "text-slate-700 dark:text-slate-300",
};

registerText("connected pill", STATUS_CONNECTED_TEXT, STATUS_CONNECTED_BG);
registerText("connecting pill", STATUS_CONNECTING_TEXT, STATUS_CONNECTING_BG);
registerText("offline pill", STATUS_OFFLINE_TEXT, STATUS_OFFLINE_BG);

// ---------------------------------------------------------------------------------------------
// Buttons and inputs
// ---------------------------------------------------------------------------------------------

/** The primary submit/CTA button: one fixed background+text color pair in both schemes
 * (white on sky-700, unchanged from light to dark — `bg-sky-600` with white text measured
 * 4.02:1, under AA; `sky-700` measures 5.86:1). Owns its own text color so no caller writes a
 * raw `text-white` literal. */
export const primaryButtonBg = "bg-sky-700 text-white";
export const primaryButtonHoverBg = "hover:bg-sky-800";
export const primaryButtonEnabledHoverBg = "enabled:hover:bg-sky-800";
export const PRIMARY_BUTTON_DISABLED_BG = pair(P.SLATE_300, P.SLATE_700);
export const primaryButtonDisabled =
  "disabled:cursor-not-allowed disabled:bg-slate-300 dark:disabled:bg-slate-700 dark:disabled:text-slate-400";

registerText(
  "primary button text (resting)",
  { dark: P.WHITE, light: P.WHITE },
  { dark: P.SKY_700, light: P.SKY_700 }
);
registerText(
  "primary button text (hover)",
  { dark: P.WHITE, light: P.WHITE },
  { dark: P.SKY_800, light: P.SKY_800 }
);

export const secondaryButtonText = "text-slate-700 dark:text-slate-300";
export const secondaryButtonBorder = "border-slate-300 dark:border-slate-700";
export const secondaryButtonHoverBorder = "hover:border-sky-500 dark:hover:border-sky-400";
export const secondaryButtonDisabledText = "disabled:text-slate-400 dark:disabled:text-slate-600";

/** An input/select/textarea's resting border and background, plus its focus border. Pass
 * `recessed: true` when the control sits inside a `SURFACE` card (its dark background needs to
 * read one level below the card, i.e. `SURFACE_RECESSED`); leave it `false` when the control
 * sits directly on `CANVAS` (its dark background reads one level above the canvas, i.e. plain
 * `SURFACE`). Both branches are complete literal strings — Tailwind's scanner sees both,
 * regardless of which one is returned at runtime. */
export function inputClasses(recessed: boolean): string {
  return recessed
    ? "border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-950 dark:focus:border-sky-400"
    : "border-slate-300 bg-white focus:border-sky-600 dark:border-slate-700 dark:bg-slate-900 dark:focus:border-sky-400";
}

// ---------------------------------------------------------------------------------------------
// The navigation rail: a deliberately constant-dark surface (unlike everything else in this
// file, it does not vary with `prefers-color-scheme` — it is a fixed brand rail, the same
// design GitHub/Linear/Notion use for their sidebars). Each export below is a single Tailwind
// class, not a light/dark pair; contrast is still registered so a future rail color change
// can't silently drop below AA.
// ---------------------------------------------------------------------------------------------

const RAIL_BG_SWATCH = P.SLATE_950;
const railFixed = (swatch: Swatch): ColorPair => ({ dark: swatch, light: swatch });

export const railBg = "bg-slate-950";
export const railBorder = "border-slate-800";
export const railHoverBg = "hover:bg-slate-800";
export const railText = "text-slate-100";
export const railSecondaryText = "text-slate-300";
export const railMutedText = "text-slate-400";
export const railAccentText = "text-sky-300";
export const railAccentHoverText = "hover:text-sky-200";
export const railHoverText = "hover:text-slate-100";
export const railActiveBg = "bg-slate-800";
export const railFocusOverlayBg = "focus:bg-slate-950";
export const railFocusOverlayText = "focus:text-slate-100";
export const railDangerText = "text-rose-300";
export const railBadgeBg = "bg-sky-500";
export const railBadgeText = "text-slate-950";

registerText("rail primary text", railFixed(P.SLATE_100), railFixed(RAIL_BG_SWATCH));
registerText("rail secondary text", railFixed(P.SLATE_300), railFixed(RAIL_BG_SWATCH));
registerText("rail muted text", railFixed(P.SLATE_400), railFixed(RAIL_BG_SWATCH));
registerText("rail link text", railFixed(P.SKY_300), railFixed(RAIL_BG_SWATCH));
registerText("rail error text", railFixed(P.ROSE_300), railFixed(RAIL_BG_SWATCH));
registerText("rail unread-count badge text", railFixed(P.SLATE_950), railFixed(P.SKY_500));
/** `railActiveBg` (a nav item's selected/hover state) is a lighter shade than the base rail —
 * secondary/accent rail text sitting on it needs its own check, since it's a different
 * background from `RAIL_BG_SWATCH` above. */
const RAIL_ACTIVE_BG_SWATCH = P.SLATE_800;
registerText(
  "rail accent text on rail active background",
  railFixed(P.SKY_300),
  railFixed(RAIL_ACTIVE_BG_SWATCH)
);
registerText(
  "rail secondary text on rail active background",
  railFixed(P.SLATE_300),
  railFixed(RAIL_ACTIVE_BG_SWATCH)
);

/** A modal/drawer/sheet backdrop: a fixed translucent black, unchanged between schemes (a
 * backdrop is never itself read as text or a surface color — no contrast check applies). */
export const backdrop40 = "bg-slate-950/40";
export const backdrop50 = "bg-slate-950/50";

/** A layout-preserving invisible border: reserves the border's width so a hover/focus state
 * that adds a real border doesn't shift the element, or so a loading skeleton matches its live
 * counterpart's box size exactly. Transparent has no color to differ between schemes — no
 * `dark:` variant, no contrast check. */
export const borderTransparent = "border-transparent";
/** An invisible fill, same reasoning as `borderTransparent` (an editable field that only shows
 * its surface color once something else — a hover border, a focus ring — signals it's live). */
export const bgTransparent = "bg-transparent";
/** Hides text while reserving its layout space, e.g. a skeleton placeholder revealing its own
 * pulsing background instead of the real (not-yet-loaded) text. Same reasoning as
 * `borderTransparent`. */
export const textTransparent = "text-transparent";

/** The active-tab underline/indicator border: identical values to `FOCUS_ACCENT`, reused under
 * its own name since a tab indicator is a different semantic role even though it happens to
 * share a palette entry. */
export const activeTabIndicatorBorder = "border-sky-600 dark:border-sky-400";
export const activeTabIndicatorText = "text-sky-700 dark:text-sky-400";

/** A raw code/text block inside a danger callout (the Mermaid render-error preview): the
 * block's own light background stays `SURFACE`, but in dark mode it reuses the enclosing
 * `CALLOUT_DANGER_BG`/`CALLOUT_DANGER_TEXT` shades rather than `SURFACE`'s, so the block doesn't
 * read as a jarring plain-white/dark-slate rectangle floating inside a rose callout. Genuinely
 * asymmetric (its light and dark sides come from two different roles) — no new contrast check
 * is registered either, since its light half is exactly "primary text on surface" (already
 * checked above) and its dark half is exactly "danger callout text"'s dark side (already
 * checked above). */
export const errorCodeBlockBg = "bg-white dark:bg-rose-950";
export const errorCodeBlockText = "text-slate-900 dark:text-rose-300";

/** A highlighted artifact card's accent ring: fixed both schemes, purely decorative (the card
 * also gets a background/border change elsewhere), so no contrast check applies. */
export const highlightRing = "ring-2 ring-sky-400";

/** A composer reference chip's border: fixed both schemes (its `sky-800`/`sky-300` text is
 * `badgeMed.text`'s own values, reused directly; its hover border is `controlHoverBorder`). */
export const referencePillBorder = "border-sky-300";

/** The "new since you last read" divider's rule lines: purely decorative (a 1px-tall bar), no
 * contrast check applies. */
export const newDividerLine = "bg-sky-200 dark:bg-sky-900";

// ---------------------------------------------------------------------------------------------
// Sizing-agnostic composites the components import directly
// ---------------------------------------------------------------------------------------------

export const card = "border-slate-200 dark:border-slate-800 bg-white dark:bg-slate-900";
export const canvasText = "bg-slate-50 dark:bg-slate-950 text-slate-900 dark:text-slate-100";
export const surfaceText = "bg-white dark:bg-slate-900 text-slate-900 dark:text-slate-100";

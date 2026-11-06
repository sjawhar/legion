import { PHONE_VIEWPORT_QUERY, useMediaQuery } from "../shell/useDialog";
import { useUserPreference } from "../shell/userPreference";

/**
 * Whether the children section is open, kept for the next issue the reader opens
 * (dispatch://INFRA-1196 R10), the way the panel's property sections are kept
 * (`useMarginSections.ts`): a browser preference of the signed-in identity
 * (`features/shell/userPreference.ts`).
 *
 * A reader who has never chosen gets the width's own answer: open from Tailwind's `md`, where
 * the rows cost the page a third of a screen, and closed below it, where seven of them cost a
 * phone the whole of one and would put the first open question off the first screen (R6). What
 * is stored is the reader's own press, either way, so a choice made on a laptop holds on a
 * phone and a choice made on a phone holds on a laptop.
 */
export interface ChildrenSection {
  readonly isOpen: boolean;
  readonly toggle: () => void;
}

/** What the reader chose, or `null` while they have chosen nothing. */
type Choice = "collapsed" | "open" | null;

export function useChildrenSection(): ChildrenSection {
  const [choice, setChoice] = useUserPreference<Choice>(
    "issue.children-section",
    (stored) => (stored === "collapsed" || stored === "open" ? stored : null),
    (value) => value ?? ""
  );
  const isPhoneViewport = useMediaQuery(PHONE_VIEWPORT_QUERY);
  const isOpen = choice === null ? !isPhoneViewport : choice === "open";
  return { isOpen, toggle: () => setChoice(isOpen ? "collapsed" : "open") };
}

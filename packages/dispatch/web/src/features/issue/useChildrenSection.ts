import { useUserPreference } from "../shell/userPreference";

/**
 * Whether the children section is open, kept for the next issue the reader opens
 * (dispatch://INFRA-1196 R10), the way the panel's property sections are kept
 * (`useMarginSections.ts`): a browser preference of the signed-in identity
 * (`features/shell/userPreference.ts`).
 *
 * The section opens expanded, so a first load shows the children; what is stored is the
 * reader's collapse, which is the only thing they chose.
 */
export interface ChildrenSection {
  readonly isOpen: boolean;
  readonly toggle: () => void;
}

export function useChildrenSection(): ChildrenSection {
  const [collapsed, setCollapsed] = useUserPreference<boolean>(
    "issue.children-section",
    (stored) => stored === "collapsed",
    (value) => (value ? "collapsed" : "open")
  );
  return { isOpen: !collapsed, toggle: () => setCollapsed(!collapsed) };
}
